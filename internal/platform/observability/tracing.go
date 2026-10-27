package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/xshubhamg/chore-microservices/internal/platform/config"
)

// SetupTracing installs a global tracer provider. When no OTLP endpoint is
// configured it is a no-op, so local development and tests do not need a
// collector.
func SetupTracing(ctx context.Context, cfg config.Config) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if cfg.Telemetry.OTLPEndpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	options := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Telemetry.OTLPEndpoint),
	}
	if cfg.Telemetry.Insecure {
		options = append(options, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", cfg.Service),
			attribute.String("service.version", cfg.Version),
			attribute.String("deployment.environment", cfg.Env),
		),
		resource.WithHost(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Telemetry.SampleRatio))),
	)
	otel.SetTracerProvider(provider)

	return provider.Shutdown, nil
}

// TraceParent serialises the active span context into a W3C traceparent header
// value. The outbox stores it so the relay can link async processing back to
// the HTTP request that caused it.
func TraceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// StartLinkedSpan starts a producer span that links to the span identified by
// traceparent. Used by the outbox relay: the publish is a new operation, but
// trace UIs can still jump back to the originating request.
func StartLinkedSpan(ctx context.Context, tracer trace.Tracer, name, traceparent string) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindProducer)}

	if traceparent != "" {
		extracted := otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
		if sc := trace.SpanContextFromContext(extracted); sc.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}))
		}
	}

	return tracer.Start(ctx, name, opts...)
}
