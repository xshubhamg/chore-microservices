// Package postgres owns the pgx connection pool lifecycle.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xshubhamg/chore-microservices/internal/platform/config"
)

// Connect builds a pool and blocks until the database accepts queries (or the
// configured connect timeout elapses). Waiting for the database makes
// `docker compose up` deterministic without sleeping in entrypoints.
func Connect(ctx context.Context, cfg config.DBConfig, appName string, log *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.ConnConfig.RuntimeParams["application_name"] = appName

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	deadline := time.Now().Add(cfg.ConnectTimeout)
	backoff := 200 * time.Millisecond

	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			log.Info("database connected", slog.Int("attempt", attempt))
			return pool, nil
		}
		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("database not reachable after %s: %w", cfg.ConnectTimeout, err)
		}

		log.Warn("database not ready, retrying",
			slog.Int("attempt", attempt),
			slog.Duration("retry_in", backoff),
			slog.String("error", err.Error()),
		)

		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

// Check returns a readiness probe for the pool.
func Check(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		return pool.Ping(ctx)
	}
}
