// Command booking owns bookings. It validates the selected service over gRPC,
// persists the booking and its event in one transaction, and lets the outbox
// relay publish the event to RabbitMQ.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	bookingapp "github.com/xshubhamg/chore-microservices/internal/booking/app"
	"github.com/xshubhamg/chore-microservices/internal/booking/infra/catalogclient"
	"github.com/xshubhamg/chore-microservices/internal/booking/infra/eventbus"
	bookingpostgres "github.com/xshubhamg/chore-microservices/internal/booking/postgres"
	bookinghttp "github.com/xshubhamg/chore-microservices/internal/booking/transport/http"
	"github.com/xshubhamg/chore-microservices/internal/platform/admin"
	"github.com/xshubhamg/chore-microservices/internal/platform/config"
	"github.com/xshubhamg/chore-microservices/internal/platform/grpcx"
	"github.com/xshubhamg/chore-microservices/internal/platform/health"
	"github.com/xshubhamg/chore-microservices/internal/platform/httpx"
	"github.com/xshubhamg/chore-microservices/internal/platform/idempotency"
	"github.com/xshubhamg/chore-microservices/internal/platform/logging"
	"github.com/xshubhamg/chore-microservices/internal/platform/migrate"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
	"github.com/xshubhamg/chore-microservices/internal/platform/outbox"
	"github.com/xshubhamg/chore-microservices/internal/platform/postgres"
	"github.com/xshubhamg/chore-microservices/internal/platform/rabbitmq"
	"github.com/xshubhamg/chore-microservices/internal/platform/run"
)

func main() {
	if err := runService(); err != nil {
		slog.Error("booking stopped with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func runService() error {
	ctx := context.Background()

	cfg, err := config.Load("booking")
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
		if err := migrate.Up(ctx, cfg.DB.DSN, bookingpostgres.Migrations(), "migrations", log); err != nil {
			return err
		}
	}

	metrics := observability.New(cfg.Service, cfg.Version)

	catalogConn, err := grpcx.Dial(cfg.Catalog.GRPCAddr, log, metrics)
	if err != nil {
		return err
	}
	defer catalogConn.Close()

	broker, err := rabbitmq.Dial(ctx, cfg.AMQP.URL, log)
	if err != nil {
		return err
	}
	defer broker.Close()

	if err := broker.DeclareTopology(cfg.AMQP.Exchange); err != nil {
		return err
	}

	publisher := rabbitmq.NewPublisher(broker, cfg.AMQP.Exchange, log)
	defer publisher.Close()

	repository := bookingpostgres.New(pool)
	outboxStore := bookingpostgres.NewOutboxStore(pool)
	application := bookingapp.New(repository, catalogclient.New(catalogConn, cfg.Catalog.Timeout), log)
	idempotencyStore := idempotency.NewPostgresStore(pool)

	relay := outbox.NewRelay(outboxStore, eventbus.New(publisher), cfg.Outbox, log, metrics)

	registerOutboxGauges(metrics, outboxStore)

	checks := health.New()
	checks.Add("postgres", postgres.Check(pool))
	checks.Add("rabbitmq", broker.Ping)

	api := httpx.NewServer(cfg, log, metrics)
	bookinghttp.Register(api.App().Group("/v1"), application, cfg.HTTP, idempotencyStore, log)

	adminAPI := admin.New(cfg, checks, metrics, log)

	log.Info("booking starting",
		slog.String("http_addr", cfg.HTTP.Addr),
		slog.String("admin_addr", cfg.Admin.Addr),
		slog.String("catalog_grpc", cfg.Catalog.GRPCAddr),
		slog.String("exchange", cfg.AMQP.Exchange),
		slog.String("database", config.RedactedDSN(cfg.DB.DSN)),
	)

	group := run.New(log, cfg.ShutdownTimeout)
	group.Add("outbox-relay", relay.Run, func(context.Context) error { return nil })
	group.Add("http", api.Start, api.Stop)
	group.Add("idempotency-janitor", func(ctx context.Context) error {
		return idempotencyStore.PurgeLoop(ctx, time.Hour, log)
	}, func(context.Context) error { return nil })
	group.Add("admin", adminAPI.Start, adminAPI.Stop)

	return group.Run(ctx)
}

// registerOutboxGauges exposes the operational backlog: pending messages and
// the age of the oldest one. Alerts fire on age, not count, because a stable
// backlog is harmless while a stuck relay is not.
func registerOutboxGauges(metrics *observability.Metrics, store *bookingpostgres.OutboxStore) {
	metrics.RegisterGaugeFunc("outbox_messages_pending",
		"Number of outbox messages waiting to be published.",
		func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			count, _, err := store.Pending(ctx)
			if err != nil {
				return 0
			}
			return float64(count)
		})

	metrics.RegisterGaugeFunc("outbox_oldest_pending_seconds",
		"Age in seconds of the oldest unpublished outbox message.",
		func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, age, err := store.Pending(ctx)
			if err != nil {
				return 0
			}
			return age.Seconds()
		})
}
