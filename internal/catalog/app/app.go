// Package app contains the catalog use cases and the ports they depend on.
package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/catalog/domain"
	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
)

// Repository errors are translated to apperr by the use cases, keeping the
// persistence adapter free of HTTP/gRPC concerns.
var (
	ErrNotFound       = errors.New("service not found")
	ErrNameConflict   = errors.New("service name already exists")
	ErrVersionChanged = errors.New("service was modified by another request")
)

type ListFilter struct {
	Active  *bool
	AfterID int64
	Limit   int
}

type Repository interface {
	Create(ctx context.Context, service *domain.Service) error
	Get(ctx context.Context, id int64) (*domain.Service, error)
	List(ctx context.Context, filter ListFilter) ([]*domain.Service, error)
	Update(ctx context.Context, service *domain.Service, expectedVersion int) error
}

type App struct {
	repo  Repository
	clock func() time.Time
	log   *slog.Logger
}

func New(repo Repository, log *slog.Logger) *App {
	return &App{repo: repo, clock: time.Now, log: log}
}

type CreateInput struct {
	Name            string
	Description     string
	Category        string
	PriceCents      int64
	DurationMinutes int32
}

func (a *App) Create(ctx context.Context, in CreateInput) (*domain.Service, error) {
	service, err := domain.New(domain.Details{
		Name:            in.Name,
		Description:     in.Description,
		Category:        in.Category,
		PriceCents:      in.PriceCents,
		DurationMinutes: in.DurationMinutes,
	}, a.clock())
	if err != nil {
		return nil, err
	}

	if err := a.repo.Create(ctx, service); err != nil {
		if errors.Is(err, ErrNameConflict) {
			return nil, apperr.Conflict("service_name_taken", "an active service with this name already exists")
		}
		return nil, apperr.Internal(err)
	}

	a.log.Info("service created",
		slog.Int64("service_id", service.ID),
		slog.String("category", service.Category),
	)
	return service, nil
}

func (a *App) Get(ctx context.Context, id int64) (*domain.Service, error) {
	if id <= 0 {
		return nil, apperr.Invalid("invalid_service_id", "service id must be positive")
	}

	service, err := a.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, apperr.NotFound("service")
		}
		return nil, apperr.Internal(err)
	}
	return service, nil
}

type ListQuery struct {
	Active  *bool
	AfterID int64
	Limit   int
}

func (a *App) List(ctx context.Context, q ListQuery) ([]*domain.Service, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Limit > 100 {
		q.Limit = 100
	}
	if q.AfterID < 0 {
		return nil, apperr.Invalid("invalid_cursor", "cursor must be zero or positive")
	}

	services, err := a.repo.List(ctx, ListFilter(q))
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return services, nil
}

type UpdateInput struct {
	Name            string
	Description     string
	Category        string
	PriceCents      int64
	DurationMinutes int32
}

// Update loads the aggregate, applies the change and writes it back with
// optimistic locking. A concurrent update yields 409 instead of lost updates.
func (a *App) Update(ctx context.Context, id int64, in UpdateInput) (*domain.Service, error) {
	service, err := a.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	expectedVersion := service.Version

	if err := service.Update(domain.Details{
		Name:            in.Name,
		Description:     in.Description,
		Category:        in.Category,
		PriceCents:      in.PriceCents,
		DurationMinutes: in.DurationMinutes,
	}, a.clock()); err != nil {
		return nil, err
	}

	if err := a.repo.Update(ctx, service, expectedVersion); err != nil {
		switch {
		case errors.Is(err, ErrVersionChanged):
			return nil, apperr.Conflict("service_modified", "the service was modified by another request, reload and retry")
		case errors.Is(err, ErrNameConflict):
			return nil, apperr.Conflict("service_name_taken", "an active service with this name already exists")
		case errors.Is(err, ErrNotFound):
			return nil, apperr.NotFound("service")
		default:
			return nil, apperr.Internal(err)
		}
	}

	a.log.Info("service updated", slog.Int64("service_id", service.ID))
	return service, nil
}

func (a *App) SetActive(ctx context.Context, id int64, active bool) (*domain.Service, error) {
	service, err := a.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	expectedVersion := service.Version
	service.SetActive(active, a.clock())

	if err := a.repo.Update(ctx, service, expectedVersion); err != nil {
		switch {
		case errors.Is(err, ErrVersionChanged):
			return nil, apperr.Conflict("service_modified", "the service was modified by another request, reload and retry")
		case errors.Is(err, ErrNameConflict):
			return nil, apperr.Conflict("service_name_taken", "an active service with this name already exists")
		default:
			return nil, apperr.Internal(err)
		}
	}

	a.log.Info("service activation changed",
		slog.Int64("service_id", service.ID),
		slog.Bool("active", service.Active),
	)
	return service, nil
}
