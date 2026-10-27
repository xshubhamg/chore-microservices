package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xshubhamg/chore-microservices/internal/catalog/app"
	"github.com/xshubhamg/chore-microservices/internal/catalog/domain"
)

type Repository struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

const columns = `id, name, description, category, price_cents, duration_minutes, active, version, created_at, updated_at`

func (r *Repository) Create(ctx context.Context, service *domain.Service) error {
	err := r.pool.QueryRow(ctx, `
		INSERT INTO services (name, description, category, price_cents, duration_minutes, active)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, version, created_at, updated_at
	`,
		service.Name,
		service.Description,
		service.Category,
		service.PriceCents,
		service.DurationMinutes,
		service.Active,
	).Scan(&service.ID, &service.Version, &service.CreatedAt, &service.UpdatedAt)

	if err != nil {
		return mapWriteError(err)
	}
	return nil
}

func (r *Repository) Get(ctx context.Context, id int64) (*domain.Service, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+columns+` FROM services WHERE id = $1`, id)
	service, err := scan(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, app.ErrNotFound
		}
		return nil, fmt.Errorf("get service: %w", err)
	}
	return service, nil
}

func (r *Repository) List(ctx context.Context, filter app.ListFilter) ([]*domain.Service, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+columns+`
		FROM services
		WHERE ($1::boolean IS NULL OR active = $1)
		  AND id > $2
		ORDER BY id
		LIMIT $3
	`, filter.Active, filter.AfterID, filter.Limit)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	defer rows.Close()

	services := make([]*domain.Service, 0, filter.Limit)
	for rows.Next() {
		service, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan service: %w", err)
		}
		services = append(services, service)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate services: %w", err)
	}
	return services, nil
}

func (r *Repository) Update(ctx context.Context, service *domain.Service, expectedVersion int) error {
	err := r.pool.QueryRow(ctx, `
		UPDATE services
		SET name = $2,
		    description = $3,
		    category = $4,
		    price_cents = $5,
		    duration_minutes = $6,
		    active = $7,
		    version = version + 1,
		    updated_at = $8
		WHERE id = $1 AND version = $9
		RETURNING version, updated_at
	`,
		service.ID,
		service.Name,
		service.Description,
		service.Category,
		service.PriceCents,
		service.DurationMinutes,
		service.Active,
		service.UpdatedAt,
		expectedVersion,
	).Scan(&service.Version, &service.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if checkErr := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM services WHERE id = $1)`, service.ID).Scan(&exists); checkErr != nil {
			return fmt.Errorf("check service existence: %w", checkErr)
		}
		if !exists {
			return app.ErrNotFound
		}
		return app.ErrVersionChanged
	}
	if err != nil {
		return mapWriteError(err)
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (*domain.Service, error) {
	var service domain.Service
	err := row.Scan(
		&service.ID,
		&service.Name,
		&service.Description,
		&service.Category,
		&service.PriceCents,
		&service.DurationMinutes,
		&service.Active,
		&service.Version,
		&service.CreatedAt,
		&service.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	service.CreatedAt = service.CreatedAt.UTC()
	service.UpdatedAt = service.UpdatedAt.UTC()
	return &service, nil
}

func mapWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return app.ErrNameConflict
	}
	return fmt.Errorf("write service: %w", err)
}
