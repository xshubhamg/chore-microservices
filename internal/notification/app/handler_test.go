package app_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/notification/app"
	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/events"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
)

type fakeStore struct {
	processed bool
	err       error

	marked   []app.Notification
	envelope events.Envelope
}

func (s *fakeStore) IsProcessed(context.Context, string) (bool, error) {
	return s.processed, s.err
}

func (s *fakeStore) MarkProcessed(_ context.Context, envelope events.Envelope, notification app.Notification) error {
	s.envelope = envelope
	s.marked = append(s.marked, notification)
	return s.err
}

type fakeSender struct {
	err  error
	sent []app.Notification
}

func (s *fakeSender) Send(_ context.Context, notification app.Notification) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, notification)
	return nil
}

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newHandler(store app.Store, sender app.Sender) *app.Handler {
	return app.NewHandler(store, sender, observability.New("test", "test"), logger())
}

func bookingEnvelope(t *testing.T, eventType string, payload events.BookingPayload) events.Envelope {
	t.Helper()

	envelope, err := events.NewAt(eventType, events.ProducerBooking, time.Now().UTC(), payload)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	return envelope
}

func TestHandleCreatedEvent(t *testing.T) {
	store := &fakeStore{}
	sender := &fakeSender{}

	envelope := bookingEnvelope(t, events.TypeBookingCreated, events.BookingPayload{
		BookingID:    42,
		ServiceName:  "Deep clean",
		CustomerName: "Ada",
		Email:        "ada@example.com",
		ScheduledAt:  time.Now().Add(48 * time.Hour),
	})

	if err := newHandler(store, sender).Handle(context.Background(), envelope); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(sender.sent) != 1 {
		t.Fatalf("sent %d notifications, want 1", len(sender.sent))
	}
	if sender.sent[0].Kind != "booking_created" {
		t.Errorf("kind = %q", sender.sent[0].Kind)
	}
	if sender.sent[0].Recipient != "ada@example.com" {
		t.Errorf("recipient = %q", sender.sent[0].Recipient)
	}
	if len(store.marked) != 1 {
		t.Fatalf("marked %d events, want 1", len(store.marked))
	}
	if store.envelope.ID != envelope.ID {
		t.Errorf("marked event %q, want %q", store.envelope.ID, envelope.ID)
	}
}

func TestHandleDuplicateIsSkipped(t *testing.T) {
	store := &fakeStore{processed: true}
	sender := &fakeSender{}

	envelope := bookingEnvelope(t, events.TypeBookingConfirmed, events.BookingPayload{BookingID: 1})
	if err := newHandler(store, sender).Handle(context.Background(), envelope); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(sender.sent) != 0 {
		t.Error("duplicate event must not send a notification")
	}
	if len(store.marked) != 0 {
		t.Error("duplicate event must not be marked again")
	}
}

func TestHandleCancelledEventIncludesReason(t *testing.T) {
	sender := &fakeSender{}
	envelope := bookingEnvelope(t, events.TypeBookingCancelled, events.BookingPayload{
		BookingID:    7,
		ServiceName:  "Plumbing",
		CustomerName: "Ada",
		Email:        "ada@example.com",
		Reason:       "plans changed",
	})

	if err := newHandler(&fakeStore{}, sender).Handle(context.Background(), envelope); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(sender.sent) != 1 || sender.sent[0].Kind != "booking_cancelled" {
		t.Fatalf("sent = %+v", sender.sent)
	}
}

func TestHandleUnsupportedEventIsPermanent(t *testing.T) {
	envelope := bookingEnvelope(t, "user.deleted", events.BookingPayload{})
	err := newHandler(&fakeStore{}, &fakeSender{}).Handle(context.Background(), envelope)

	if apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("kind = %q, want invalid (dead-letter)", apperr.KindOf(err))
	}
}

func TestHandleMalformedPayloadIsPermanent(t *testing.T) {
	envelope := events.Envelope{
		ID:         "0b6f8f7e-3f4e-4f26-8f8e-7b8b6d9e3a11",
		Type:       events.TypeBookingCreated,
		Version:    1,
		OccurredAt: time.Now().UTC(),
		Producer:   events.ProducerBooking,
		Payload:    json.RawMessage(`"not-an-object"`),
	}

	err := newHandler(&fakeStore{}, &fakeSender{}).Handle(context.Background(), envelope)
	if apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("kind = %q, want invalid", apperr.KindOf(err))
	}
}

func TestHandleSenderFailureIsRetried(t *testing.T) {
	sender := &fakeSender{err: io.ErrUnexpectedEOF}
	store := &fakeStore{}

	envelope := bookingEnvelope(t, events.TypeBookingCreated, events.BookingPayload{BookingID: 3})
	err := newHandler(store, sender).Handle(context.Background(), envelope)

	if err == nil {
		t.Fatal("expected sender error")
	}
	if apperr.KindOf(err) != apperr.KindInternal {
		t.Fatalf("kind = %q, want internal (retryable)", apperr.KindOf(err))
	}
	if len(store.marked) != 0 {
		t.Error("event must not be marked processed when sending fails")
	}
}
