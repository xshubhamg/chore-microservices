// Package events defines the integration events exchanged between services and
// the envelope they travel in.
//
// The envelope is deliberately boring and stable. Consumers can route on Type,
// deduplicate on ID and evolve Payload using Version without touching the
// transport.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	TypeBookingCreated   = "booking.created"
	TypeBookingConfirmed = "booking.confirmed"
	TypeBookingCompleted = "booking.completed"
	TypeBookingCancelled = "booking.cancelled"
)

const (
	ProducerBooking = "booking"
)

// Envelope is the wire format of every message published to the event bus.
type Envelope struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Version    int             `json:"version"`
	OccurredAt time.Time       `json:"occurred_at"`
	Producer   string          `json:"producer"`
	Payload    json.RawMessage `json:"payload"`
}

// New builds an envelope with a fresh event ID and the current schema version.
func New(eventType, producer string, payload any) (Envelope, error) {
	return NewAt(eventType, producer, time.Now().UTC(), payload)
}

// NewAt is New with an injectable clock, which keeps tests deterministic.
func NewAt(eventType, producer string, occurredAt time.Time, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	return Envelope{
		ID:         uuid.NewString(),
		Type:       eventType,
		Version:    1,
		OccurredAt: occurredAt.UTC(),
		Producer:   producer,
		Payload:    raw,
	}, nil
}

func (e Envelope) Validate() error {
	if _, err := uuid.Parse(e.ID); err != nil {
		return fmt.Errorf("event id: %w", err)
	}
	if e.Type == "" {
		return errors.New("event type is required")
	}
	if e.Producer == "" {
		return errors.New("event producer is required")
	}
	if e.OccurredAt.IsZero() {
		return errors.New("event occurred_at is required")
	}
	if len(e.Payload) == 0 {
		return errors.New("event payload is required")
	}
	return nil
}

func (e Envelope) Marshal() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

func Parse(data []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// BookingPayload is shared by all booking.* events. Fields are additive: new
// optional fields do not require a version bump.
type BookingPayload struct {
	BookingID    int64     `json:"booking_id"`
	ServiceID    int64     `json:"service_id"`
	ServiceName  string    `json:"service_name"`
	CustomerName string    `json:"customer_name"`
	Email        string    `json:"customer_email"`
	ScheduledAt  time.Time `json:"scheduled_at"`
	Status       string    `json:"status"`
	Version      int       `json:"booking_version"`
	Reason       string    `json:"reason,omitempty"`
}
