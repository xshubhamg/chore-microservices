// Command catalog serves the chore catalogue over REST (public API) and gRPC
// (internal API used by the booking service).
package main

import (
	"context"
	"log/slog"
	"os"

	grpc_health "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	catalogapp "github.com/xshubhamg/chore-microservices/internal/catalog/app"
	catalogpostgres "github.com/xshubhamg/chore-microservices/internal/catalog/postgres"
	catalogrpc "github.com/xshubhamg/chore-microservices/internal/catalog/transport/grpc"
	cataloghttp "github.com/xshubhamg/chore-microservices/internal/catalog/transport/http"
	catalogv1 "github.com/xshubhamg/chore-microservices/internal/gen/catalog/v1"
	"github.com/xshubhamg/chore-microservices/internal/platform/admin"
	"github.com/xshubhamg/chore-microservices/internal/platform/config"
	"github.com/xshubhamg/chore-microservices/internal/platform/grpcx"
	"github.com/xshubhamg/chore-microservices/internal/platform/health"
	"github.com/xshubhamg/chore-microservices/internal/platform/httpx"
	"github.com/xshubhamg/chore-microservices/internal/platform/logging"
	"github.com/xshubhamg/chore-microservices/internal/platform/migrate"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
	"github.com/xshubhamg/chore-microservices/internal/platform/postgres"
	"github.com/xshubhamg/chore-microservices/internal/platform/run"
)

func main() {
	if err := runService(); err != nil {
		slog.Error("catalog stopped with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func runService() error {
	ctx := context.Background()

	cfg, err := config.Load("catalog")
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
		if err := migrate.Up(ctx, cfg.DB.DSN, catalogpostgres.Migrations(), "migrations", log); err != nil {
			return err
		}
	}

	metrics := observability.New(cfg.Service, cfg.Version)
	application := catalogapp.New(catalogpostgres.New(pool), log)

	checks := health.New()
	checks.Add("postgres", postgres.Check(pool))

	api := httpx.NewServer(cfg, log, metrics)
	cataloghttp.Register(api.App().Group("/v1"), application, cfg.HTTP)

	grpcAPI := grpcx.NewServer(cfg.GRPC.Addr, log, metrics)
	catalogv1.RegisterCatalogServiceServer(grpcAPI.GRPC(), catalogrpc.NewServer(application))

	grpcHealth := grpc_health.NewServer()
	grpcHealth.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcAPI.GRPC(), grpcHealth)

	adminAPI := admin.New(cfg, checks, metrics, log)

	log.Info("catalog starting",
		slog.String("http_addr", cfg.HTTP.Addr),
		slog.String("grpc_addr", cfg.GRPC.Addr),
		slog.String("admin_addr", cfg.Admin.Addr),
		slog.String("database", config.RedactedDSN(cfg.DB.DSN)),
	)

	group := run.New(log, cfg.ShutdownTimeout)
	group.Add("http", api.Start, api.Stop)
	group.Add("grpc", grpcAPI.Start, grpcAPI.Stop)
	group.Add("admin", adminAPI.Start, adminAPI.Stop)

	return group.Run(ctx)
}
