package domain_test

import (
	"testing"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/catalog/domain"
	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
)

var now = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func validDetails() domain.Details {
	return domain.Details{
		Name:            "  Deep clean  ",
		Description:     "Two cleaners, three hours",
		Category:        "CLEANING",
		PriceCents:      12900,
		DurationMinutes: 180,
	}
}

func TestNewServiceNormalisesInput(t *testing.T) {
	t.Parallel()

	service, err := domain.New(validDetails(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if service.Name != "Deep clean" {
		t.Errorf("name = %q, want trimmed name", service.Name)
	}
	if service.Category != "cleaning" {
		t.Errorf("category = %q, want normalised category", service.Category)
	}
	if !service.Active {
		t.Error("new services must be active")
	}
	if service.Version != 1 {
		t.Errorf("version = %d, want 1", service.Version)
	}
}

func TestNewServiceValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*domain.Details)
		field  string
	}{
		{"short name", func(d *domain.Details) { d.Name = "ab" }, "name"},
		{"long description", func(d *domain.Details) { d.Description = string(make([]byte, 1001)) }, "description"},
		{"unknown category", func(d *domain.Details) { d.Category = "astrology" }, "category"},
		{"negative price", func(d *domain.Details) { d.PriceCents = -1 }, "price_cents"},
		{"duration too short", func(d *domain.Details) { d.DurationMinutes = 5 }, "duration_minutes"},
		{"duration too long", func(d *domain.Details) { d.DurationMinutes = 5000 }, "duration_minutes"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			details := validDetails()
			tc.mutate(&details)

			_, err := domain.New(details, now)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if apperr.KindOf(err) != apperr.KindInvalid {
				t.Fatalf("kind = %q, want invalid", apperr.KindOf(err))
			}
			if _, ok := apperr.From(err).Details[tc.field]; !ok {
				t.Errorf("missing detail %q", tc.field)
			}
		})
	}
}

func TestSetActiveIsIdempotent(t *testing.T) {
	t.Parallel()

	service, err := domain.New(validDetails(), now)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	service.SetActive(false, now.Add(time.Hour))
	if service.Active {
		t.Error("service should be inactive")
	}
	updatedAt := service.UpdatedAt

	service.SetActive(false, now.Add(2*time.Hour))
	if !service.UpdatedAt.Equal(updatedAt) {
		t.Error("repeated deactivation should not touch updated_at")
	}
}
