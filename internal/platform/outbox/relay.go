// Package outbox implements the transactional-outbox relay.
//
// A service writes the domain change and an outbox row in one database
// transaction. A separate relay publishes pending rows to the broker with
// confirms and marks them published. If the process dies between publish and
// mark, the message is published again; consumers deduplicate by event ID. That
// is at-least-once delivery without a distributed transaction.
package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/xshubhamg/chore-microservices/internal/platform/config"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
)

type Message struct {
	ID          int64
	EventID     string
	EventType   string
	Payload     []byte
	TraceParent string
	Attempts    int
}

type Store interface {
	// Claim atomically selects pending rows and marks them as claimed until
	// now+lease. Claimed rows are retried when the lease expires, so a crashed
	// relay cannot strand a message.
	Claim(ctx context.Context, limit int, lease time.Duration) ([]Message, error)
	MarkPublished(ctx context.Context, id int64) error
	MarkRetry(ctx context.Context, id int64, retryAt time.Time, cause string) error
	MarkDead(ctx context.Context, id int64, cause string) error

	// Pending returns the backlog size and the age of the oldest pending event.
	Pending(ctx context.Context) (int64, time.Duration, error)
	PurgePublished(ctx context.Context, olderThan time.Time) (int64, error)
}

type Publisher interface {
	Publish(ctx context.Context, msg Message) error
}

type Relay struct {
	store      Store
	publisher  Publisher
	cfg        config.OutboxConfig
	log        *slog.Logger
	metrics    *observability.Metrics
	tracer     trace.Tracer
	purgeAfter time.Duration
}

func NewRelay(store Store, publisher Publisher, cfg config.OutboxConfig, log *slog.Logger, metrics *observability.Metrics) *Relay {
	return &Relay{
		store:      store,
		publisher:  publisher,
		cfg:        cfg,
		log:        log,
		metrics:    metrics,
		tracer:     otel.Tracer("outbox"),
		purgeAfter: 7 * 24 * time.Hour,
	}
}

// Run polls the outbox until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	lastPurge := time.Now()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.drain(ctx); err != nil {
				r.log.Error("outbox drain failed", slog.String("error", err.Error()))
			}
			if time.Since(lastPurge) > time.Hour {
				lastPurge = time.Now()
				if n, err := r.store.PurgePublished(ctx, time.Now().Add(-r.purgeAfter)); err != nil {
					r.log.Warn("purge published outbox messages failed", slog.String("error", err.Error()))
				} else if n > 0 {
					r.log.Info("purged published outbox messages", slog.Int64("rows", n))
				}
			}
		}
	}
}

func (r *Relay) drain(ctx context.Context) error {
	for {
		// The lease is longer than a full batch publish, so rows are not stolen
		// from a healthy relay.
		lease := 2 * time.Minute
		messages, err := r.store.Claim(ctx, r.cfg.BatchSize, lease)
		if err != nil {
			return fmt.Errorf("claim outbox messages: %w", err)
		}
		if len(messages) == 0 {
			return nil
		}

		for _, message := range messages {
			r.publishOne(ctx, message)
		}

		if len(messages) < r.cfg.BatchSize {
			return nil
		}
	}
}

func (r *Relay) publishOne(ctx context.Context, message Message) {
	ctx, span := observability.StartLinkedSpan(ctx, r.tracer, "outbox.publish "+message.EventType, message.TraceParent)
	defer span.End()
	span.SetAttributes(
		attribute.String("event.type", message.EventType),
		attribute.String("event.id", message.EventID),
		attribute.Int("outbox.attempt", message.Attempts),
	)

	start := time.Now()
	err := r.publisher.Publish(ctx, message)
	r.metrics.OutboxPublishDuration.Observe(time.Since(start).Seconds())

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		if message.Attempts >= r.cfg.MaxAttempts {
			r.metrics.ObservePublished(message.EventType, "dead")
			if markErr := r.store.MarkDead(ctx, message.ID, err.Error()); markErr != nil {
				r.log.Error("mark outbox message dead failed",
					slog.Int64("outbox_id", message.ID), slog.String("error", markErr.Error()))
			}
			r.log.Error("outbox message moved to dead letter",
				slog.Int64("outbox_id", message.ID),
				slog.String("event_type", message.EventType),
				slog.String("error", err.Error()),
			)
			return
		}

		retryAt := time.Now().Add(r.backoff(message.Attempts))
		r.metrics.ObservePublished(message.EventType, "retry")
		if markErr := r.store.MarkRetry(ctx, message.ID, retryAt, err.Error()); markErr != nil {
			r.log.Error("schedule outbox retry failed",
				slog.Int64("outbox_id", message.ID), slog.String("error", markErr.Error()))
		}
		r.log.Warn("outbox publish failed, scheduled retry",
			slog.Int64("outbox_id", message.ID),
			slog.Int("attempt", message.Attempts),
			slog.Time("retry_at", retryAt),
			slog.String("error", err.Error()),
		)
		return
	}

	r.metrics.ObservePublished(message.EventType, "success")
	if err := r.store.MarkPublished(ctx, message.ID); err != nil {
		// The event is already on the broker. Leaving it pending causes a
		// duplicate publish, which consumers absorb via event-ID deduplication.
		r.log.Error("mark outbox message published failed",
			slog.Int64("outbox_id", message.ID), slog.String("error", err.Error()))
	}
}

// backoff returns an exponentially growing delay with +/-20% jitter, capped at
// OUTBOX_MAX_BACKOFF. Jitter prevents a fleet of relays from retrying in sync.
func (r *Relay) backoff(attempt int) time.Duration {
	delay := float64(r.cfg.BaseBackoff) * math.Pow(2, float64(max(attempt-1, 0)))
	if delay > float64(r.cfg.MaxBackoff) {
		delay = float64(r.cfg.MaxBackoff)
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(delay * jitter)
}
