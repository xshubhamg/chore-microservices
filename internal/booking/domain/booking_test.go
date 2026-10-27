package domain_test

import (
	"testing"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/booking/domain"
	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
)

var now = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func validInput() domain.NewInput {
	return domain.NewInput{
		ServiceID:         7,
		ServiceName:       "Deep clean",
		ServicePriceCents: 8900,
		DurationMinutes:   120,
		CustomerName:      "Ada Lovelace",
		CustomerEmail:     "ada@example.com",
		ScheduledAt:       now.Add(48 * time.Hour),
	}
}

func TestNewBooking(t *testing.T) {
	t.Parallel()

	booking, err := domain.New(validInput(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if booking.Status != domain.StatusPending {
		t.Errorf("status = %q, want pending", booking.Status)
	}
	if booking.Version != 1 {
		t.Errorf("version = %d, want 1", booking.Version)
	}
	if !booking.ScheduledAt.Equal(now.Add(48 * time.Hour)) {
		t.Errorf("scheduled_at = %v, want %v", booking.ScheduledAt, now.Add(48*time.Hour))
	}
	if booking.ServicePriceCents != 8900 || booking.ServiceName != "Deep clean" {
		t.Errorf("catalog snapshot not copied: %+v", booking)
	}
}

func TestNewBookingValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*domain.NewInput)
		field  string
	}{
		{
			name:   "missing service",
			mutate: func(in *domain.NewInput) { in.ServiceID = 0 },
			field:  "service_id",
		},
		{
			name:   "short customer name",
			mutate: func(in *domain.NewInput) { in.CustomerName = "A" },
			field:  "customer_name",
		},
		{
			name:   "invalid email",
			mutate: func(in *domain.NewInput) { in.CustomerEmail = "not-an-email" },
			field:  "customer_email",
		},
		{
			name:   "scheduled too soon",
			mutate: func(in *domain.NewInput) { in.ScheduledAt = now.Add(5 * time.Minute) },
			field:  "scheduled_at",
		},
		{
			name:   "scheduled too far away",
			mutate: func(in *domain.NewInput) { in.ScheduledAt = now.Add(400 * 24 * time.Hour) },
			field:  "scheduled_at",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := validInput()
			tc.mutate(&in)

			_, err := domain.New(in, now)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if got := apperr.KindOf(err); got != apperr.KindInvalid {
				t.Fatalf("kind = %q, want %q", got, apperr.KindInvalid)
			}
			appErr := apperr.From(err)
			if _, ok := appErr.Details[tc.field]; !ok {
				t.Errorf("missing detail %q in %v", tc.field, appErr.Details)
			}
		})
	}
}

func TestStatusMachine(t *testing.T) {
	t.Parallel()

	booking, err := domain.New(validInput(), now)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := booking.Confirm(now.Add(time.Minute)); err != nil {
		t.Fatalf("pending -> confirmed: %v", err)
	}
	if err := booking.Complete(now.Add(2 * time.Minute)); err != nil {
		t.Fatalf("confirmed -> completed: %v", err)
	}

	if err := booking.Cancel("too late", now.Add(3*time.Minute)); err == nil {
		t.Fatal("completed -> cancelled should fail")
	} else if apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("kind = %q, want conflict", apperr.KindOf(err))
	}

	if !booking.IsFinal() {
		t.Error("completed booking should be final")
	}
}

func TestCancelFromPending(t *testing.T) {
	t.Parallel()

	booking, err := domain.New(validInput(), now)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := booking.Cancel("", now.Add(time.Hour)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if booking.Status != domain.StatusCancelled {
		t.Errorf("status = %q, want cancelled", booking.Status)
	}
	if booking.CancellationReason != "cancelled by customer" {
		t.Errorf("reason = %q, want default reason", booking.CancellationReason)
	}
}

func TestCancelTwiceIsConflict(t *testing.T) {
	t.Parallel()

	booking, err := domain.New(validInput(), now)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := booking.Cancel("first", now); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	if err := booking.Cancel("second", now); err == nil {
		t.Fatal("second cancel should conflict")
	}
}
