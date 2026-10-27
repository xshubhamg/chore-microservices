// Package domain holds booking invariants and the state machine.
package domain

import (
	"net/mail"
	"strings"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusConfirmed Status = "confirmed"
	StatusCompleted Status = "completed"
	StatusCancelled Status = "cancelled"
)

const (
	minLeadTime      = 15 * time.Minute
	maxBookingWindow = 365 * 24 * time.Hour
	maxReasonLength  = 500
)

// transitions is the allowed booking state machine.
var transitions = map[Status][]Status{
	StatusPending:   {StatusConfirmed, StatusCancelled},
	StatusConfirmed: {StatusCompleted, StatusCancelled},
	StatusCompleted: {},
	StatusCancelled: {},
}

type Booking struct {
	ID int64

	// Service fields are a snapshot taken from the catalog when the booking is
	// created. Historical bookings must not change when the catalog changes.
	ServiceID         int64
	ServiceName       string
	ServicePriceCents int64
	DurationMinutes   int32

	CustomerName  string
	CustomerEmail string
	ScheduledAt   time.Time

	Status             Status
	CancellationReason string

	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

type NewInput struct {
	ServiceID         int64
	ServiceName       string
	ServicePriceCents int64
	DurationMinutes   int32
	CustomerName      string
	CustomerEmail     string
	ScheduledAt       time.Time
}

func New(in NewInput, now time.Time) (*Booking, error) {
	in.CustomerName = strings.TrimSpace(in.CustomerName)
	in.CustomerEmail = strings.ToLower(strings.TrimSpace(in.CustomerEmail))
	in.ScheduledAt = in.ScheduledAt.UTC()

	err := apperr.Invalid("invalid_booking", "booking details are invalid")

	if in.ServiceID <= 0 {
		err.WithDetail("service_id", "must be positive")
	}
	if strings.TrimSpace(in.ServiceName) == "" {
		err.WithDetail("service_id", "service not found")
	}
	if in.ServicePriceCents < 0 {
		err.WithDetail("service_id", "service price is invalid")
	}
	if in.DurationMinutes <= 0 {
		err.WithDetail("service_id", "service duration is invalid")
	}

	switch {
	case len(in.CustomerName) < 2:
		err.WithDetail("customer_name", "must be at least 2 characters")
	case len(in.CustomerName) > 100:
		err.WithDetail("customer_name", "must be at most 100 characters")
	}

	if !validEmail(in.CustomerEmail) {
		err.WithDetail("customer_email", "must be a valid email address")
	}

	switch {
	case in.ScheduledAt.Before(now.Add(minLeadTime)):
		err.WithDetail("scheduled_at", "must be at least 15 minutes in the future")
	case in.ScheduledAt.After(now.Add(maxBookingWindow)):
		err.WithDetail("scheduled_at", "must be within the next 365 days")
	}

	if len(err.Details) > 0 {
		return nil, err
	}

	return &Booking{
		ServiceID:         in.ServiceID,
		ServiceName:       in.ServiceName,
		ServicePriceCents: in.ServicePriceCents,
		DurationMinutes:   in.DurationMinutes,
		CustomerName:      in.CustomerName,
		CustomerEmail:     in.CustomerEmail,
		ScheduledAt:       in.ScheduledAt,
		Status:            StatusPending,
		Version:           1,
		CreatedAt:         now.UTC(),
		UpdatedAt:         now.UTC(),
	}, nil
}

func (b *Booking) Confirm(now time.Time) error {
	return b.transition(StatusConfirmed, now)
}

func (b *Booking) Complete(now time.Time) error {
	return b.transition(StatusCompleted, now)
}

func (b *Booking) Cancel(reason string, now time.Time) error {
	reason = strings.TrimSpace(reason)
	if len(reason) > maxReasonLength {
		return apperr.Invalid("invalid_cancellation_reason", "cancellation reason must be at most 500 characters")
	}
	if reason == "" {
		reason = "cancelled by customer"
	}

	if err := b.transition(StatusCancelled, now); err != nil {
		return err
	}
	b.CancellationReason = reason
	return nil
}

func (b *Booking) transition(to Status, now time.Time) error {
	for _, allowed := range transitions[b.Status] {
		if allowed == to {
			b.Status = to
			b.UpdatedAt = now.UTC()
			return nil
		}
	}

	return apperr.Conflict("invalid_status_transition", "booking cannot move from "+string(b.Status)+" to "+string(to)).
		WithDetail("from", string(b.Status)).
		WithDetail("to", string(to))
}

func (b *Booking) IsFinal() bool {
	return b.Status == StatusCompleted || b.Status == StatusCancelled
}

func validEmail(value string) bool {
	if value == "" {
		return false
	}
	addr, err := mail.ParseAddress(value)
	if err != nil || addr.Address != value {
		return false
	}
	at := strings.LastIndex(value, "@")
	return at > 0 && strings.Contains(value[at+1:], ".")
}
