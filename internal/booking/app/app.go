// Package app contains the booking use cases. Dependencies are expressed as
// small interfaces so the use cases can be tested without Postgres, RabbitMQ or
// a live catalog service.
package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/booking/domain"
	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/events"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
)

var (
	ErrNotFound       = errors.New("booking not found")
	ErrVersionChanged = errors.New("booking was modified by another request")
)

type ListFilter struct {
	Status  *domain.Status
	AfterID int64
	Limit   int
}

type Repository interface {
	// Create persists the booking and its domain event in one transaction.
	// buildEvent runs after the database assigns the booking ID, so the event
	// payload always contains the final identifier.
	Create(ctx context.Context, booking *domain.Booking, buildEvent func() (events.Outbox, error)) error
	Get(ctx context.Context, id int64) (*domain.Booking, error)
	List(ctx context.Context, filter ListFilter) ([]*domain.Booking, error)
	// Update applies an optimistic-locking update and appends the event.
	Update(ctx context.Context, booking *domain.Booking, expectedVersion int, event events.Outbox) error
}

// CatalogService is the local read model of a catalog service.
type CatalogService struct {
	ID              int64
	Name            string
	PriceCents      int64
	DurationMinutes int32
	Active          bool
}

// ServiceCatalog is the anti-corruption boundary around the catalog service.
type ServiceCatalog interface {
	GetService(ctx context.Context, id int64) (CatalogService, error)
}

type Option func(*App)

func WithClock(clock func() time.Time) Option {
	return func(a *App) { a.clock = clock }
}

type App struct {
	repo    Repository
	catalog ServiceCatalog
	clock   func() time.Time
	log     *slog.Logger
}

func New(repo Repository, catalog ServiceCatalog, log *slog.Logger, opts ...Option) *App {
	a := &App{repo: repo, catalog: catalog, clock: time.Now, log: log}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

type CreateInput struct {
	ServiceID     int64
	CustomerName  string
	CustomerEmail string
	ScheduledAt   time.Time
}

func (a *App) Create(ctx context.Context, in CreateInput) (*domain.Booking, error) {
	if in.ServiceID <= 0 {
		return nil, apperr.Invalid("invalid_service_id", "service_id must be a positive integer")
	}

	service, err := a.catalog.GetService(ctx, in.ServiceID)
	if err != nil {
		switch apperr.KindOf(err) {
		case apperr.KindNotFound:
			return nil, apperr.New(apperr.KindNotFound, "service_not_found", "service not found")
		case apperr.KindUnavailable:
			return nil, apperr.Unavailable("catalog_unavailable", "catalog service is unavailable, please retry")
		default:
			return nil, err
		}
	}

	if !service.Active {
		return nil, apperr.Invalid("service_inactive", "service is not available for booking").
			WithDetail("service_id", "inactive")
	}

	booking, err := domain.New(domain.NewInput{
		ServiceID:         service.ID,
		ServiceName:       service.Name,
		ServicePriceCents: service.PriceCents,
		DurationMinutes:   service.DurationMinutes,
		CustomerName:      in.CustomerName,
		CustomerEmail:     in.CustomerEmail,
		ScheduledAt:       in.ScheduledAt,
	}, a.clock())
	if err != nil {
		return nil, err
	}

	if err := a.repo.Create(ctx, booking, func() (events.Outbox, error) {
		return a.event(ctx, events.TypeBookingCreated, booking)
	}); err != nil {
		return nil, apperr.Internal(err)
	}

	a.log.Info("booking created",
		slog.Int64("booking_id", booking.ID),
		slog.Int64("service_id", booking.ServiceID),
		slog.Time("scheduled_at", booking.ScheduledAt),
	)
	return booking, nil
}

func (a *App) Get(ctx context.Context, id int64) (*domain.Booking, error) {
	if id <= 0 {
		return nil, apperr.Invalid("invalid_booking_id", "booking id must be positive")
	}

	booking, err := a.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, apperr.NotFound("booking")
		}
		return nil, apperr.Internal(err)
	}
	return booking, nil
}

type ListQuery struct {
	Status  *domain.Status
	AfterID int64
	Limit   int
}

func (a *App) List(ctx context.Context, q ListQuery) ([]*domain.Booking, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Limit > 100 {
		q.Limit = 100
	}
	if q.AfterID < 0 {
		return nil, apperr.Invalid("invalid_cursor", "cursor must be zero or positive")
	}
	if q.Status != nil && !validStatus(*q.Status) {
		return nil, apperr.Invalid("invalid_status", "status must be pending, confirmed, completed or cancelled").
			WithDetail("status", string(*q.Status))
	}

	bookings, err := a.repo.List(ctx, ListFilter(q))
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return bookings, nil
}

func (a *App) Confirm(ctx context.Context, id int64) (*domain.Booking, error) {
	booking, err := a.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	expectedVersion := booking.Version
	if err := booking.Confirm(a.clock()); err != nil {
		return nil, err
	}
	return a.persist(ctx, booking, expectedVersion, events.TypeBookingConfirmed)
}

func (a *App) Complete(ctx context.Context, id int64) (*domain.Booking, error) {
	booking, err := a.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	expectedVersion := booking.Version
	if err := booking.Complete(a.clock()); err != nil {
		return nil, err
	}
	return a.persist(ctx, booking, expectedVersion, events.TypeBookingCompleted)
}

func (a *App) Cancel(ctx context.Context, id int64, reason string) (*domain.Booking, error) {
	booking, err := a.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	expectedVersion := booking.Version
	if err := booking.Cancel(reason, a.clock()); err != nil {
		return nil, err
	}
	return a.persist(ctx, booking, expectedVersion, events.TypeBookingCancelled)
}

func (a *App) persist(ctx context.Context, booking *domain.Booking, expectedVersion int, eventType string) (*domain.Booking, error) {
	event, err := a.event(ctx, eventType, booking)
	if err != nil {
		return nil, err
	}

	if err := a.repo.Update(ctx, booking, expectedVersion, event); err != nil {
		switch {
		case errors.Is(err, ErrVersionChanged):
			return nil, apperr.Conflict("booking_modified", "the booking was modified by another request, reload and retry")
		case errors.Is(err, ErrNotFound):
			return nil, apperr.NotFound("booking")
		default:
			return nil, apperr.Internal(err)
		}
	}

	a.log.Info("booking updated",
		slog.Int64("booking_id", booking.ID),
		slog.String("status", string(booking.Status)),
		slog.Int("version", booking.Version),
	)
	return booking, nil
}

func (a *App) event(ctx context.Context, eventType string, booking *domain.Booking) (events.Outbox, error) {
	envelope, err := events.NewAt(eventType, events.ProducerBooking, a.clock(), events.BookingPayload{
		BookingID:    booking.ID,
		ServiceID:    booking.ServiceID,
		ServiceName:  booking.ServiceName,
		CustomerName: booking.CustomerName,
		Email:        booking.CustomerEmail,
		ScheduledAt:  booking.ScheduledAt,
		Status:       string(booking.Status),
		Version:      booking.Version,
		Reason:       booking.CancellationReason,
	})
	if err != nil {
		return events.Outbox{}, apperr.Internal(err)
	}
	return events.Outbox{Envelope: envelope, TraceParent: observability.TraceParent(ctx)}, nil
}

func validStatus(status domain.Status) bool {
	switch status {
	case domain.StatusPending, domain.StatusConfirmed, domain.StatusCompleted, domain.StatusCancelled:
		return true
	default:
		return false
	}
}
