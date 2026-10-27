package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Message is a publish request. MessageID should be the event ID so consumers
// can deduplicate retries.
type Message struct {
	RoutingKey string
	MessageID  string
	Timestamp  time.Time
	Headers    amqp.Table
	Body       []byte
}

// Publisher publishes with confirms enabled: Publish only returns nil once the
// broker has acknowledged the message. Combined with the transactional outbox,
// that gives at-least-once delivery without losing events during broker
// restarts.
type Publisher struct {
	client   *Client
	exchange string
	log      *slog.Logger

	mu       sync.Mutex
	ch       *amqp.Channel
	confirms chan amqp.Confirmation
}

func NewPublisher(client *Client, exchange string, log *slog.Logger) *Publisher {
	return &Publisher{client: client, exchange: exchange, log: log}
}

func (p *Publisher) Publish(ctx context.Context, msg Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		if err := p.ensureChannel(); err != nil {
			lastErr = err
			p.log.Warn("recreate publisher channel", slog.String("error", err.Error()))
			continue
		}

		err := p.ch.PublishWithContext(ctx, p.exchange, msg.RoutingKey, false, false, amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    msg.MessageID,
			Type:         msg.RoutingKey,
			Timestamp:    msg.Timestamp,
			Headers:      msg.Headers,
			Body:         msg.Body,
		})
		if err != nil {
			lastErr = fmt.Errorf("publish %s: %w", msg.RoutingKey, err)
			p.reset()
			continue
		}

		select {
		case confirm, ok := <-p.confirms:
			if !ok {
				lastErr = errors.New("publisher confirm channel closed")
				p.reset()
				continue
			}
			if !confirm.Ack {
				lastErr = errors.New("broker rejected message (nack)")
				p.reset()
				continue
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(confirmTimeout):
			lastErr = errors.New("timed out waiting for publisher confirm")
			p.reset()
			continue
		}
	}
	return lastErr
}

func (p *Publisher) ensureChannel() error {
	if p.ch != nil && !p.ch.IsClosed() {
		return nil
	}

	ch, err := p.client.Channel()
	if err != nil {
		return err
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return fmt.Errorf("enable publisher confirms: %w", err)
	}

	p.ch = ch
	p.confirms = ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	return nil
}

func (p *Publisher) reset() {
	if p.ch != nil {
		_ = p.ch.Close()
	}
	p.ch = nil
	p.confirms = nil
}

func (p *Publisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch == nil {
		return nil
	}
	err := p.ch.Close()
	p.ch = nil
	return err
}
