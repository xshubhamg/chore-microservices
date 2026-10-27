package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/logging"
)

// ConsumerConfig describes a durable quorum queue bound to the event exchange.
type ConsumerConfig struct {
	Queue         string
	RoutingKeys   []string
	Exchange      string
	Prefetch      int
	DeliveryLimit int
	Handler       func(context.Context, amqp.Delivery) error
	HandleTimeout time.Duration
}

type Consumer struct {
	client *Client
	cfg    ConsumerConfig
	log    *slog.Logger
}

func NewConsumer(client *Client, cfg ConsumerConfig, log *slog.Logger) *Consumer {
	if cfg.DeliveryLimit <= 0 {
		cfg.DeliveryLimit = 5
	}
	if cfg.HandleTimeout <= 0 {
		cfg.HandleTimeout = 20 * time.Second
	}
	if cfg.Prefetch <= 0 {
		cfg.Prefetch = 20
	}
	return &Consumer{client: client, cfg: cfg, log: log}
}

// Run consumes until ctx is cancelled. Broker disconnects are retried with
// exponential backoff; the queue declaration is idempotent.
func (c *Consumer) Run(ctx context.Context) error {
	backoff := time.Second

	for {
		if ctx.Err() != nil {
			return nil
		}

		started := time.Now()
		err := c.consume(ctx)
		if ctx.Err() != nil {
			return nil
		}

		// A long healthy run resets the backoff so a single blip does not
		// keep us waiting.
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}

		c.log.Warn("consumer disconnected, reconnecting",
			slog.String("error", err.Error()),
			slog.Duration("retry_in", backoff),
		)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Consumer) consume(ctx context.Context) error {
	ch, err := c.client.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	defer ch.Close()

	if err := c.declare(ch); err != nil {
		return err
	}

	if err := ch.Qos(c.cfg.Prefetch, 0, false); err != nil {
		return fmt.Errorf("set prefetch: %w", err)
	}

	deliveries, err := ch.Consume(c.cfg.Queue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume %s: %w", c.cfg.Queue, err)
	}

	c.log.Info("consumer ready", slog.String("queue", c.cfg.Queue))

	for {
		select {
		case <-ctx.Done():
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("delivery channel closed")
			}
			c.handle(ctx, delivery)
		}
	}
}

func (c *Consumer) declare(ch *amqp.Channel) error {
	dlq := c.cfg.Queue + ".dlq"
	dlx := DeadLetterExchange(c.cfg.Exchange)

	quorum := amqp.Table{"x-queue-type": "quorum"}

	if _, err := ch.QueueDeclare(dlq, true, false, false, false, quorum); err != nil {
		return fmt.Errorf("declare dead-letter queue: %w", err)
	}
	if err := ch.QueueBind(dlq, "", dlx, false, nil); err != nil {
		return fmt.Errorf("bind dead-letter queue: %w", err)
	}

	args := amqp.Table{
		"x-queue-type":              "quorum",
		"x-delivery-limit":          int32(c.cfg.DeliveryLimit),
		"x-dead-letter-exchange":    dlx,
		"x-dead-letter-routing-key": c.cfg.Queue,
	}
	if _, err := ch.QueueDeclare(c.cfg.Queue, true, false, false, false, args); err != nil {
		return fmt.Errorf("declare queue %s: %w", c.cfg.Queue, err)
	}
	for _, key := range c.cfg.RoutingKeys {
		if err := ch.QueueBind(c.cfg.Queue, key, c.cfg.Exchange, false, nil); err != nil {
			return fmt.Errorf("bind %s to %s: %w", c.cfg.Queue, key, err)
		}
	}
	return nil
}

func (c *Consumer) handle(parent context.Context, delivery amqp.Delivery) {
	ctx, cancel := context.WithTimeout(parent, c.cfg.HandleTimeout)
	defer cancel()

	logger := logging.WithTrace(ctx, c.log).With(
		slog.String("message_id", delivery.MessageId),
		slog.String("routing_key", delivery.RoutingKey),
	)

	err := c.cfg.Handler(ctx, delivery)
	switch {
	case err == nil:
		if ackErr := delivery.Ack(false); ackErr != nil {
			logger.Error("ack failed", slog.String("error", ackErr.Error()))
		}
	case permanent(err):
		logger.Error("message rejected, sending to dead-letter queue", slog.String("error", err.Error()))
		// requeue=false routes the message to the dead-letter exchange.
		if nackErr := delivery.Nack(false, false); nackErr != nil {
			logger.Error("nack failed", slog.String("error", nackErr.Error()))
		}
	default:
		logger.Warn("message handling failed, releasing for redelivery", slog.String("error", err.Error()))
		// requeue=true increments the delivery count; RabbitMQ moves the
		// message to the DLQ once x-delivery-limit is exhausted.
		if nackErr := delivery.Nack(false, true); nackErr != nil {
			logger.Error("nack failed", slog.String("error", nackErr.Error()))
		}
	}
}

// permanent reports whether retrying the message cannot succeed. Malformed
// messages and validation errors are dead-lettered immediately.
func permanent(err error) bool {
	kind := apperr.KindOf(err)
	return kind == apperr.KindInvalid || kind == apperr.KindNotFound || kind == apperr.KindForbidden
}
