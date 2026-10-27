// Command notification consumes booking events and "sends" customer emails.
// The sender is a port, so production can plug in SES/SendGrid without touching
// the consumer or the deduplication logic.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	notificationapp "github.com/xshubhamg/chore-microservices/internal/notification/app"
	notificationpostgres "github.com/xshubhamg/chore-microservices/internal/notification/postgres"
	notificationamqp "github.com/xshubhamg/chore-microservices/internal/notification/transport/amqp"
	"github.com/xshubhamg/chore-microservices/internal/platform/admin"
	"github.com/xshubhamg/chore-microservices/internal/platform/config"
	"github.com/xshubhamg/chore-microservices/internal/platform/health"
	"github.com/xshubhamg/chore-microservices/internal/platform/logging"
	"github.com/xshubhamg/chore-microservices/internal/platform/migrate"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
	"github.com/xshubhamg/chore-microservices/internal/platform/postgres"
	"github.com/xshubhamg/chore-microservices/internal/platform/rabbitmq"
	"github.com/xshubhamg/chore-microservices/internal/platform/run"
)

const (
	queueName     = "notification.booking-events"
	deliveryLimit = 5
)

func main() {
	if err := runService(); err != nil {
		slog.Error("notification stopped with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func runService() error {
	ctx := context.Background()

	cfg, err := config.Load("notification")
	if err != nil {
		return err
	}

	log := logging.New(cfg)
	slog.SetDefault(log)

	shutdownTracing, err := observability.SetupTracing(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	pool, err := postgres.Connect(ctx, cfg.DB, cfg.Service, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	if cfg.AutoMigrate {
		if err := migrate.Up(ctx, cfg.DB.DSN, notificationpostgres.Migrations(), "migrations", log); err != nil {
			return err
		}
	}

	metrics := observability.New(cfg.Service, cfg.Version)

	broker, err := rabbitmq.Dial(ctx, cfg.AMQP.URL, log)
	if err != nil {
		return err
	}
	defer func() { _ = broker.Close() }()

	if err := broker.DeclareTopology(cfg.AMQP.Exchange); err != nil {
		return err
	}

	store := notificationpostgres.New(pool)
	application := notificationapp.NewHandler(store, notificationapp.NewLogSender(log), metrics, log)
	handler := notificationamqp.NewHandler(application, log)

	checks := health.New()
	checks.Add("postgres", postgres.Check(pool))
	checks.Add("rabbitmq", broker.Ping)

	adminAPI := admin.New(cfg, checks, metrics, log)

	consumer := rabbitmq.NewConsumer(broker, rabbitmq.ConsumerConfig{
		Queue:         queueName,
		RoutingKeys:   []string{"booking.*"},
		Exchange:      cfg.AMQP.Exchange,
		Prefetch:      cfg.AMQP.Prefetch,
		DeliveryLimit: deliveryLimit,
		HandleTimeout: 30 * time.Second,
		Handler:       handler.Handle,
	}, log)

	log.Info("notification starting",
		slog.String("queue", queueName),
		slog.String("exchange", cfg.AMQP.Exchange),
		slog.String("admin_addr", cfg.Admin.Addr),
		slog.String("database", config.RedactedDSN(cfg.DB.DSN)),
	)

	group := run.New(log, cfg.ShutdownTimeout)
	group.Add("consumer", consumer.Run, func(context.Context) error { return nil })
	group.Add("admin", adminAPI.Start, adminAPI.Stop)

	return group.Run(ctx)
}
