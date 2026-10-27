package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/catalog/app"
	"github.com/xshubhamg/chore-microservices/internal/catalog/domain"
	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
)

var fixedTime = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

type fakeRepository struct {
	createErr error
	getErr    error
	updateErr error

	service  *domain.Service
	filter   app.ListFilter
	services []*domain.Service
}

func (r *fakeRepository) Create(_ context.Context, service *domain.Service) error {
	if r.createErr != nil {
		return r.createErr
	}
	service.ID = 11
	return nil
}

func (r *fakeRepository) Get(context.Context, int64) (*domain.Service, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.service == nil {
		return nil, app.ErrNotFound
	}
	copy := *r.service
	return &copy, nil
}

func (r *fakeRepository) List(_ context.Context, filter app.ListFilter) ([]*domain.Service, error) {
	r.filter = filter
	return r.services, nil
}

func (r *fakeRepository) Update(_ context.Context, _ *domain.Service, _ int) error {
	return r.updateErr
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func validInput() app.CreateInput {
	return app.CreateInput{
		Name:            "Deep clean",
		Description:     "Two cleaners",
		Category:        "cleaning",
		PriceCents:      12900,
		DurationMinutes: 180,
	}
}

func TestCreateReturnsServiceWithID(t *testing.T) {
	repo := &fakeRepository{}
	application := app.New(repo, testLogger())

	service, err := application.Create(context.Background(), validInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if service.ID != 11 {
		t.Errorf("id = %d, want 11", service.ID)
	}
	if service.Category != "cleaning" {
		t.Errorf("category = %q", service.Category)
	}
}

func TestCreateMapsDuplicateName(t *testing.T) {
	repo := &fakeRepository{createErr: app.ErrNameConflict}
	application := app.New(repo, testLogger())

	_, err := application.Create(context.Background(), validInput())
	if apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("kind = %q, want conflict", apperr.KindOf(err))
	}
	if code := apperr.From(err).Code; code != "service_name_taken" {
		t.Errorf("code = %q", code)
	}
}

func TestGetMapsNotFound(t *testing.T) {
	application := app.New(&fakeRepository{}, testLogger())

	_, err := application.Get(context.Background(), 99)
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("kind = %q, want not_found", apperr.KindOf(err))
	}
}

func TestUpdateMapsVersionConflict(t *testing.T) {
	service, err := domain.New(domain.Details{
		Name:            "Deep clean",
		Category:        "cleaning",
		PriceCents:      12900,
		DurationMinutes: 180,
	}, fixedTime)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	service.ID = 11
	service.Version = 4

	repo := &fakeRepository{service: service, updateErr: app.ErrVersionChanged}
	application := app.New(repo, testLogger())

	_, err = application.Update(context.Background(), 11, app.UpdateInput{
		Name:            "Deep clean plus",
		Category:        "cleaning",
		PriceCents:      15900,
		DurationMinutes: 180,
	})
	if apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("kind = %q, want conflict", apperr.KindOf(err))
	}
	if code := apperr.From(err).Code; code != "service_modified" {
		t.Errorf("code = %q", code)
	}
}

func TestListClampsLimit(t *testing.T) {
	repo := &fakeRepository{}
	application := app.New(repo, testLogger())

	if _, err := application.List(context.Background(), app.ListQuery{Limit: 500}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if repo.filter.Limit != 100 {
		t.Errorf("limit = %d, want 100", repo.filter.Limit)
	}
}

func TestCreatePropagatesInternalError(t *testing.T) {
	repo := &fakeRepository{createErr: errors.New("connection reset")}
	application := app.New(repo, testLogger())

	_, err := application.Create(context.Background(), validInput())
	if apperr.KindOf(err) != apperr.KindInternal {
		t.Fatalf("kind = %q, want internal", apperr.KindOf(err))
	}
}
