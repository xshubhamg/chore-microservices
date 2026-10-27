// Package eventbus adapts the outbox relay to the RabbitMQ publisher.
package eventbus

import (
	"context"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
	"github.com/xshubhamg/chore-microservices/internal/platform/outbox"
	"github.com/xshubhamg/chore-microservices/internal/platform/rabbitmq"
)

type Publisher struct {
	publisher *rabbitmq.Publisher
}

func New(publisher *rabbitmq.Publisher) *Publisher {
	return &Publisher{publisher: publisher}
}

// Publish converts a stored outbox message into a persistent AMQP message.
// The trace context injected here belongs to the relay's publish span, which is
// linked to the HTTP request that produced the event.
func (p *Publisher) Publish(ctx context.Context, message outbox.Message) error {
	headers := amqp.Table{}
	observability.InjectAMQP(ctx, headers)

	return p.publisher.Publish(ctx, rabbitmq.Message{
		RoutingKey: message.EventType,
		MessageID:  message.EventID,
		Timestamp:  time.Now().UTC(),
		Headers:    headers,
		Body:       message.Payload,
	})
}
