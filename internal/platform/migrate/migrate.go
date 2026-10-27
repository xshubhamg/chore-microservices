// Package migrate applies embedded, versioned SQL migrations at service start.
//
// Trade-off: running migrations in-process keeps the demo to a single command
// and avoids a separate migration job. golang-migrate takes a Postgres advisory
// lock, so concurrent replicas are safe. For a stricter production process,
// run `cmd/migrate` as a Kubernetes Job before rolling out.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Up applies every pending migration found in fsys under dir.
func Up(ctx context.Context, dsn string, fsys fs.FS, dir string, log *slog.Logger) error {
	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}

	db := stdlib.OpenDB(*connCfg)
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("create migration driver: %w", err)
	}

	source, err := iofs.New(fsys, dir)
	if err != nil {
		return fmt.Errorf("load migrations from %q: %w", dir, err)
	}

	migrator, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}

	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}

	version, dirty, err := migrator.Version()
	if err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	log.Info("migrations applied", slog.Uint64("version", uint64(version)), slog.Bool("dirty", dirty))
	return nil
}
