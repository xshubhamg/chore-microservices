package app

import (
	"context"
	"log/slog"

	"github.com/xshubhamg/chore-microservices/internal/platform/logging"
)

// LogSender is the local adapter. Swapping it for an SES/SendGrid adapter is a
// one-file change because the port is small.
type LogSender struct {
	log *slog.Logger
}

func NewLogSender(log *slog.Logger) *LogSender {
	return &LogSender{log: log}
}

func (s *LogSender) Send(ctx context.Context, notification Notification) error {
	logging.WithTrace(ctx, s.log).Info("outbound notification",
		slog.String("kind", notification.Kind),
		slog.String("recipient", notification.Recipient),
		slog.String("subject", notification.Subject),
		slog.String("body", notification.Body),
	)
	return nil
}
