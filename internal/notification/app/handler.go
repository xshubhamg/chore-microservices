// Package app turns integration events into customer notifications.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/events"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
)

// Notification is the notification we would hand to an email/SMS provider.
type Notification struct {
	EventID   string
	BookingID int64
	Kind      string
	Recipient string
	Subject   string
	Body      string
	SentAt    time.Time
}

type Store interface {
	IsProcessed(ctx context.Context, eventID string) (bool, error)
	MarkProcessed(ctx context.Context, envelope events.Envelope, notification Notification) error
}

type Sender interface {
	Send(ctx context.Context, notification Notification) error
}

type Handler struct {
	store   Store
	sender  Sender
	metrics *observability.Metrics
	log     *slog.Logger
	clock   func() time.Time
}

func NewHandler(store Store, sender Sender, metrics *observability.Metrics, log *slog.Logger) *Handler {
	return &Handler{store: store, sender: sender, metrics: metrics, log: log, clock: time.Now}
}

// Handle processes one event. It is idempotent: a redelivered event is
// recognised by its UUID and skipped, because RabbitMQ guarantees
// at-least-once, not exactly-once delivery.
func (h *Handler) Handle(ctx context.Context, envelope events.Envelope) error {
	start := time.Now()
	logger := h.log.With(
		slog.String("event_id", envelope.ID),
		slog.String("event_type", envelope.Type),
	)

	var payload events.BookingPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		h.metrics.ObserveConsumed(envelope.Type, "invalid", time.Since(start))
		return apperr.Invalid("invalid_payload", "event payload is not a booking payload").WithDetail("event_id", envelope.ID)
	}

	processed, err := h.store.IsProcessed(ctx, envelope.ID)
	if err != nil {
		h.metrics.ObserveConsumed(envelope.Type, "error", time.Since(start))
		return fmt.Errorf("check processed event: %w", err)
	}
	if processed {
		h.metrics.ObserveConsumed(envelope.Type, "duplicate", time.Since(start))
		logger.Info("duplicate event ignored")
		return nil
	}

	notification, err := build(envelope.Type, payload, envelope.ID, h.clock())
	if err != nil {
		h.metrics.ObserveConsumed(envelope.Type, "invalid", time.Since(start))
		return err
	}

	if err := h.sender.Send(ctx, notification); err != nil {
		h.metrics.ObserveConsumed(envelope.Type, "error", time.Since(start))
		return fmt.Errorf("send notification: %w", err)
	}

	if err := h.store.MarkProcessed(ctx, envelope, notification); err != nil {
		h.metrics.ObserveConsumed(envelope.Type, "error", time.Since(start))
		return fmt.Errorf("record processed event: %w", err)
	}

	h.metrics.ObserveConsumed(envelope.Type, "success", time.Since(start))
	logger.Info("notification sent",
		slog.Int64("booking_id", notification.BookingID),
		slog.String("kind", notification.Kind),
	)
	return nil
}

func build(eventType string, payload events.BookingPayload, eventID string, now time.Time) (Notification, error) {
	base := Notification{
		EventID:   eventID,
		BookingID: payload.BookingID,
		Recipient: payload.Email,
		SentAt:    now.UTC(),
	}

	switch eventType {
	case events.TypeBookingCreated:
		base.Kind = "booking_created"
		base.Subject = fmt.Sprintf("Booking #%d received", payload.BookingID)
		base.Body = fmt.Sprintf(
			"Hi %s, we received your booking for %s on %s. We will notify you once a pro is assigned.",
			payload.CustomerName, payload.ServiceName, payload.ScheduledAt.Format(time.RFC1123),
		)
	case events.TypeBookingConfirmed:
		base.Kind = "booking_confirmed"
		base.Subject = fmt.Sprintf("Booking #%d confirmed", payload.BookingID)
		base.Body = fmt.Sprintf(
			"Hi %s, your %s booking on %s is confirmed.",
			payload.CustomerName, payload.ServiceName, payload.ScheduledAt.Format(time.RFC1123),
		)
	case events.TypeBookingCancelled:
		base.Kind = "booking_cancelled"
		base.Subject = fmt.Sprintf("Booking #%d cancelled", payload.BookingID)
		base.Body = fmt.Sprintf("Hi %s, your booking for %s was cancelled. Reason: %s.",
			payload.CustomerName, payload.ServiceName, payload.Reason)
	case events.TypeBookingCompleted:
		base.Kind = "booking_completed"
		base.Subject = fmt.Sprintf("Booking #%d completed", payload.BookingID)
		base.Body = fmt.Sprintf("Hi %s, your %s booking is complete. Thanks for using Chore!",
			payload.CustomerName, payload.ServiceName)
	default:
		return Notification{}, apperr.Invalid("unsupported_event", "unsupported event type "+eventType)
	}

	return base, nil
}
