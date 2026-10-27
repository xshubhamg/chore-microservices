package observability

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
)

// HTTPTracing starts a server span for every request, extracts the W3C
// traceparent header from the caller and stores the trace context in the
// request so handlers and gRPC clients continue the same trace.
func HTTPTracing(service string) fiber.Handler {
	tracer := otel.Tracer(service + "/http")

	return func(c fiber.Ctx) error {
		ctx := otel.GetTextMapPropagator().Extract(c.Context(), fiberCarrier{c})
		ctx, span := tracer.Start(ctx, c.Method()+" "+c.Path(),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", c.Method()),
				attribute.String("url.path", c.Path()),
				attribute.String("client.address", c.IP()),
			),
		)
		c.SetContext(ctx)
		defer span.End()

		err := c.Next()

		route := c.Route().Path
		if route == "" {
			route = "unmatched"
		}
		status := c.Response().StatusCode()
		if err != nil {
			status = StatusForError(err)
		}

		span.SetName(c.Method() + " " + route)
		span.SetAttributes(
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", status),
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else if status >= 500 {
			span.SetStatus(codes.Error, "server error")
		}

		return err
	}
}

// HTTPMetrics records the RED metrics (rate, errors, duration) for every
// request. The route template is used, not the raw path, to keep cardinality
// bounded.
func HTTPMetrics(m *Metrics) fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()
		err := c.Next()

		route := c.Route().Path
		if route == "" {
			route = "unmatched"
		}
		status := c.Response().StatusCode()
		if err != nil {
			status = StatusForError(err)
		}

		m.ObserveHTTP(c.Method(), route, status, time.Since(start))
		return err
	}
}

// StatusForError returns the HTTP status the central error handler will use.
// The error handler runs after middleware unwind, so middleware must derive the
// status itself to keep logs and metrics accurate.
func StatusForError(err error) int {
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return fiberErr.Code
	}
	return apperr.From(err).Kind.HTTPStatus()
}

type fiberCarrier struct{ c fiber.Ctx }

func (h fiberCarrier) Get(key string) string { return h.c.Get(key) }
func (h fiberCarrier) Set(key, value string) { h.c.Set(key, value) }
func (h fiberCarrier) Keys() []string        { return nil }
