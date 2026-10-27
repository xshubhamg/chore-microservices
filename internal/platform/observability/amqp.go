package observability

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
)

// AMQPHeaders adapts amqp.Table to the OpenTelemetry TextMapCarrier interface.
type AMQPHeaders amqp.Table

func NewAMQPHeaders() amqp.Table { return amqp.Table{} }

func (h AMQPHeaders) Get(key string) string {
	value, ok := h[key]
	if !ok {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}

func (h AMQPHeaders) Set(key, value string) { h[key] = value }

func (h AMQPHeaders) Keys() []string {
	keys := make([]string, 0, len(h))
	for key := range h {
		keys = append(keys, key)
	}
	return keys
}

// InjectAMQP writes the W3C trace headers into AMQP message headers.
func InjectAMQP(ctx context.Context, headers amqp.Table) {
	if headers == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, AMQPHeaders(headers))
}

// ExtractAMQP continues a trace from AMQP message headers.
func ExtractAMQP(ctx context.Context, headers amqp.Table) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, AMQPHeaders(headers))
}
