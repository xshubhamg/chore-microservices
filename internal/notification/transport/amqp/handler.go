// Package amqp adapts RabbitMQ deliveries to the notification handler.
package amqp

import (
	"context"
	"log/slog"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/xshubhamg/chore-microservices/internal/notification/app"
	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/events"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
)

type Handler struct {
	app    *app.Handler
	tracer trace.Tracer
	log    *slog.Logger
}

func NewHandler(application *app.Handler, log *slog.Logger) *Handler {
	return &Handler{app: application, tracer: otel.Tracer("notification/amqp"), log: log}
}

// Handle continues the trace from the message headers, decodes the envelope and
// delegates to the application layer. Malformed messages return an invalid
// error, which the consumer dead-letters instead of retrying forever.
func (h *Handler) Handle(ctx context.Context, delivery amqp.Delivery) error {
	ctx = observability.ExtractAMQP(ctx, delivery.Headers)
	ctx, span := h.tracer.Start(ctx, "process "+delivery.RoutingKey,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.destination.name", delivery.RoutingKey),
			attribute.String("messaging.message.id", delivery.MessageId),
		),
	)
	defer span.End()

	envelope, err := events.Parse(delivery.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return apperr.Invalid("invalid_event", "message is not a valid event envelope")
	}

	if err := h.app.Handle(ctx, envelope); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}
