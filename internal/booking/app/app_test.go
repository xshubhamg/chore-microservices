package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/booking/app"
	"github.com/xshubhamg/chore-microservices/internal/booking/domain"
	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/events"
)

var fixedTime = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

type fakeRepository struct {
	createErr error
	getErr    error
	updateErr error

	stored          *domain.Booking
	createdBooking  *domain.Booking
	createdEvent    events.Outbox
	updatedEvent    events.Outbox
	expectedVersion int
	updateCalls     int
}

func (r *fakeRepository) Create(_ context.Context, booking *domain.Booking, buildEvent func() (events.Outbox, error)) error {
	if r.createErr != nil {
		return r.createErr
	}
	booking.ID = 42
	event, err := buildEvent()
	if err != nil {
		return err
	}
	r.createdBooking = booking
	r.createdEvent = event
	return nil
}

func (r *fakeRepository) Get(context.Context, int64) (*domain.Booking, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.stored == nil {
		return nil, app.ErrNotFound
	}
	copy := *r.stored
	return &copy, nil
}

func (r *fakeRepository) List(context.Context, app.ListFilter) ([]*domain.Booking, error) {
	return nil, nil
}

func (r *fakeRepository) Update(_ context.Context, booking *domain.Booking, expectedVersion int, event events.Outbox) error {
	r.updateCalls++
	if r.updateErr != nil {
		return r.updateErr
	}
	r.expectedVersion = expectedVersion
	r.updatedEvent = event
	booking.Version = expectedVersion + 1
	return nil
}

type fakeCatalog struct {
	service app.CatalogService
	err     error
}

func (c fakeCatalog) GetService(context.Context, int64) (app.CatalogService, error) {
	return c.service, c.err
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func validCreateInput() app.CreateInput {
	return app.CreateInput{
		ServiceID:     7,
		CustomerName:  "Ada Lovelace",
		CustomerEmail: "ada@example.com",
		ScheduledAt:   fixedTime.Add(48 * time.Hour),
	}
}

func activeCatalog() fakeCatalog {
	return fakeCatalog{service: app.CatalogService{
		ID:              7,
		Name:            "Deep clean",
		PriceCents:      12900,
		DurationMinutes: 180,
		Active:          true,
	}}
}

func newApp(repo *fakeRepository, catalog fakeCatalog) *app.App {
	return app.New(repo, catalog, testLogger(), app.WithClock(func() time.Time { return fixedTime }))
}

func TestCreatePersistsSnapshotAndEvent(t *testing.T) {
	repo := &fakeRepository{}
	application := newApp(repo, activeCatalog())

	booking, err := application.Create(context.Background(), validCreateInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if booking.ID != 42 {
		t.Errorf("id = %d, want repository-assigned id", booking.ID)
	}
	if booking.ServiceName != "Deep clean" || booking.ServicePriceCents != 12900 {
		t.Errorf("catalog snapshot not stored: %+v", booking)
	}
	if booking.Status != domain.StatusPending {
		t.Errorf("status = %q, want pending", booking.Status)
	}

	if repo.createdEvent.Envelope.Type != events.TypeBookingCreated {
		t.Errorf("event type = %q, want %q", repo.createdEvent.Envelope.Type, events.TypeBookingCreated)
	}
	if err := repo.createdEvent.Envelope.Validate(); err != nil {
		t.Fatalf("event envelope invalid: %v", err)
	}

	var payload events.BookingPayload
	if err := json.Unmarshal(repo.createdEvent.Envelope.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.BookingID != 42 || payload.Email != "ada@example.com" {
		t.Errorf("payload = %+v", payload)
	}
}

func TestCreateMapsCatalogNotFound(t *testing.T) {
	application := newApp(&fakeRepository{}, fakeCatalog{
		err: apperr.New(apperr.KindNotFound, "not_found", "service not found"),
	})

	_, err := application.Create(context.Background(), validCreateInput())
	if err == nil {
		t.Fatal("expected error")
	}
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Errorf("kind = %q, want not_found", apperr.KindOf(err))
	}
	if code := apperr.From(err).Code; code != "service_not_found" {
		t.Errorf("code = %q, want service_not_found", code)
	}
}

func TestCreateMapsCatalogUnavailable(t *testing.T) {
	application := newApp(&fakeRepository{}, fakeCatalog{
		err: apperr.Unavailable("upstream_unavailable", "catalog down"),
	})

	_, err := application.Create(context.Background(), validCreateInput())
	if apperr.KindOf(err) != apperr.KindUnavailable {
		t.Errorf("kind = %q, want unavailable", apperr.KindOf(err))
	}
}

func TestCreateRejectsInactiveService(t *testing.T) {
	catalog := activeCatalog()
	catalog.service.Active = false

	_, err := newApp(&fakeRepository{}, catalog).Create(context.Background(), validCreateInput())
	if apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("kind = %q, want invalid", apperr.KindOf(err))
	}
	if code := apperr.From(err).Code; code != "service_inactive" {
		t.Errorf("code = %q, want service_inactive", code)
	}
}

func TestCreateWrapsRepositoryError(t *testing.T) {
	repo := &fakeRepository{createErr: errors.New("disk on fire")}

	_, err := newApp(repo, activeCatalog()).Create(context.Background(), validCreateInput())
	if apperr.KindOf(err) != apperr.KindInternal {
		t.Fatalf("kind = %q, want internal", apperr.KindOf(err))
	}
}

func TestCancelPublishesEvent(t *testing.T) {
	stored, err := domain.New(domain.NewInput{
		ServiceID:         7,
		ServiceName:       "Deep clean",
		ServicePriceCents: 12900,
		DurationMinutes:   180,
		CustomerName:      "Ada Lovelace",
		CustomerEmail:     "ada@example.com",
		ScheduledAt:       fixedTime.Add(48 * time.Hour),
	}, fixedTime)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	stored.ID = 42
	stored.Version = 3

	repo := &fakeRepository{stored: stored}
	booking, err := newApp(repo, activeCatalog()).Cancel(context.Background(), 42, "plans changed")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if booking.Status != domain.StatusCancelled {
		t.Errorf("status = %q, want cancelled", booking.Status)
	}
	if repo.expectedVersion != 3 {
		t.Errorf("expected version = %d, want 3", repo.expectedVersion)
	}
	if repo.updatedEvent.Envelope.Type != events.TypeBookingCancelled {
		t.Errorf("event type = %q, want %q", repo.updatedEvent.Envelope.Type, events.TypeBookingCancelled)
	}
}

func TestCancelCompletedBookingIsConflict(t *testing.T) {
	stored, err := domain.New(domain.NewInput{
		ServiceID:         7,
		ServiceName:       "Deep clean",
		ServicePriceCents: 12900,
		DurationMinutes:   180,
		CustomerName:      "Ada Lovelace",
		CustomerEmail:     "ada@example.com",
		ScheduledAt:       fixedTime.Add(48 * time.Hour),
	}, fixedTime)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	stored.Status = domain.StatusCompleted

	repo := &fakeRepository{stored: stored}
	_, err = newApp(repo, activeCatalog()).Cancel(context.Background(), 42, "")
	if apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("kind = %q, want conflict", apperr.KindOf(err))
	}
	if repo.updateCalls != 0 {
		t.Error("repository must not be called for an invalid transition")
	}
}

func TestVersionConflictIsReportedAsConflict(t *testing.T) {
	stored, err := domain.New(domain.NewInput{
		ServiceID:         7,
		ServiceName:       "Deep clean",
		ServicePriceCents: 12900,
		DurationMinutes:   180,
		CustomerName:      "Ada Lovelace",
		CustomerEmail:     "ada@example.com",
		ScheduledAt:       fixedTime.Add(48 * time.Hour),
	}, fixedTime)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	repo := &fakeRepository{stored: stored, updateErr: app.ErrVersionChanged}
	_, err = newApp(repo, activeCatalog()).Cancel(context.Background(), 42, "")
	if apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("kind = %q, want conflict", apperr.KindOf(err))
	}
	if code := apperr.From(err).Code; code != "booking_modified" {
		t.Errorf("code = %q, want booking_modified", code)
	}
}

func TestListClampsLimit(t *testing.T) {
	repo := &fakeRepository{}

	var captured app.ListFilter
	repoList := &capturingRepository{fakeRepository: repo, captured: &captured}
	application := app.New(repoList, activeCatalog(), testLogger())

	if _, err := application.List(context.Background(), app.ListQuery{Limit: 500}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if captured.Limit != 100 {
		t.Errorf("limit = %d, want clamped to 100", captured.Limit)
	}
}

type capturingRepository struct {
	*fakeRepository
	captured *app.ListFilter
}

func (r *capturingRepository) List(_ context.Context, filter app.ListFilter) ([]*domain.Booking, error) {
	*r.captured = filter
	return nil, nil
}
