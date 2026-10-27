package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/xshubhamg/chore-microservices/internal/platform/config"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
)

type fakeStore struct {
	batches   [][]Message
	claimCall int

	published []int64
	retried   map[int64]time.Time
	dead      []int64
	deadCause string
}

func (s *fakeStore) Claim(context.Context, int, time.Duration) ([]Message, error) {
	s.claimCall++
	if s.claimCall > len(s.batches) {
		return nil, nil
	}
	return s.batches[s.claimCall-1], nil
}

func (s *fakeStore) MarkPublished(_ context.Context, id int64) error {
	s.published = append(s.published, id)
	return nil
}

func (s *fakeStore) MarkRetry(_ context.Context, id int64, retryAt time.Time, _ string) error {
	if s.retried == nil {
		s.retried = map[int64]time.Time{}
	}
	s.retried[id] = retryAt
	return nil
}

func (s *fakeStore) MarkDead(_ context.Context, id int64, cause string) error {
	s.dead = append(s.dead, id)
	s.deadCause = cause
	return nil
}

func (s *fakeStore) Pending(context.Context) (int64, time.Duration, error) { return 0, 0, nil }

func (s *fakeStore) PurgePublished(context.Context, time.Time) (int64, error) { return 0, nil }

type fakePublisher struct {
	err       error
	published []Message
}

func (p *fakePublisher) Publish(_ context.Context, message Message) error {
	if p.err != nil {
		return p.err
	}
	p.published = append(p.published, message)
	return nil
}

func newRelay(store Store, publisher Publisher) *Relay {
	cfg := config.OutboxConfig{
		PollInterval: time.Millisecond,
		BatchSize:    10,
		MaxAttempts:  3,
		BaseBackoff:  time.Second,
		MaxBackoff:   time.Minute,
	}
	metrics := observability.New("test", "test")
	return NewRelay(store, publisher, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics)
}

func TestDrainPublishesAndMarksMessages(t *testing.T) {
	store := &fakeStore{batches: [][]Message{{
		{ID: 1, EventID: "e1", EventType: "booking.created", Payload: []byte(`{}`), Attempts: 1},
		{ID: 2, EventID: "e2", EventType: "booking.created", Payload: []byte(`{}`), Attempts: 1},
	}}}
	publisher := &fakePublisher{}

	if err := newRelay(store, publisher).drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(publisher.published) != 2 {
		t.Fatalf("published %d messages, want 2", len(publisher.published))
	}
	if len(store.published) != 2 || store.published[0] != 1 || store.published[1] != 2 {
		t.Errorf("marked published = %v, want [1 2]", store.published)
	}
	if len(store.retried) != 0 || len(store.dead) != 0 {
		t.Errorf("unexpected retry/dead: %v %v", store.retried, store.dead)
	}
}

func TestDrainSchedulesRetryOnFailure(t *testing.T) {
	store := &fakeStore{batches: [][]Message{{
		{ID: 5, EventID: "e5", EventType: "booking.created", Payload: []byte(`{}`), Attempts: 1},
	}}}
	publisher := &fakePublisher{err: errors.New("broker unavailable")}

	before := time.Now()
	if err := newRelay(store, publisher).drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	retryAt, ok := store.retried[5]
	if !ok {
		t.Fatal("message was not scheduled for retry")
	}
	if retryAt.Before(before.Add(800*time.Millisecond)) || retryAt.After(before.Add(2*time.Second)) {
		t.Errorf("retryAt = %v, want roughly now+1s with jitter", retryAt)
	}
	if len(store.published) != 0 {
		t.Errorf("failed message must not be marked published")
	}
}

func TestDrainMarksDeadAfterMaxAttempts(t *testing.T) {
	store := &fakeStore{batches: [][]Message{{
		{ID: 9, EventID: "e9", EventType: "booking.created", Payload: []byte(`{}`), Attempts: 3},
	}}}
	publisher := &fakePublisher{err: errors.New("still down")}

	if err := newRelay(store, publisher).drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(store.dead) != 1 || store.dead[0] != 9 {
		t.Fatalf("dead = %v, want [9]", store.dead)
	}
	if store.deadCause == "" {
		t.Error("dead message should retain the last error")
	}
	if len(store.retried) != 0 {
		t.Error("message at max attempts must not be retried")
	}
}

func TestBackoffIsCappedAndJittered(t *testing.T) {
	relay := newRelay(&fakeStore{}, &fakePublisher{})

	first := relay.backoff(1)
	if first < 800*time.Millisecond || first > 1200*time.Millisecond {
		t.Errorf("first backoff = %v, want ~1s", first)
	}

	capped := relay.backoff(20)
	if capped < 48*time.Second || capped > 72*time.Second {
		t.Errorf("capped backoff = %v, want ~60s", capped)
	}
}
