// Package logging builds the structured logger used by every service. Logs are
// JSON in production so they can be shipped to Loki/ELK, and human-readable text
// in development.
package logging

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"

	"github.com/xshubhamg/chore-microservices/internal/platform/config"
)

func New(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:     cfg.Log.Level,
		AddSource: cfg.Env == config.EnvDevelopment,
	}

	var handler slog.Handler
	if cfg.Env == config.EnvDevelopment {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler).With(
		slog.String("service", cfg.Service),
		slog.String("env", cfg.Env),
		slog.String("version", cfg.Version),
	)
}

// WithTrace decorates a logger with the active trace identifiers so log lines
// can be correlated with traces in Jaeger/Tempo.
func WithTrace(ctx context.Context, logger *slog.Logger) *slog.Logger {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return logger
	}
	return logger.With(
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	)
}
