package idempotency_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/httpx"
	"github.com/xshubhamg/chore-microservices/internal/platform/idempotency"
)

type record struct {
	hash  string
	state idempotency.State
	body  *idempotency.Record
}

type memoryStore struct {
	mu      sync.Mutex
	entries map[string]*record
}

func newMemoryStore() *memoryStore {
	return &memoryStore{entries: map[string]*record{}}
}

func (s *memoryStore) Claim(_ context.Context, key, hash string, _ time.Duration) (idempotency.Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.entries[key]
	if !ok {
		s.entries[key] = &record{hash: hash, state: idempotency.StateInProgress}
		return idempotency.Claim{State: idempotency.StateNew}, nil
	}
	if existing.hash != hash {
		return idempotency.Claim{}, idempotency.ErrKeyReuse
	}
	if existing.state == idempotency.StateCompleted {
		return idempotency.Claim{State: idempotency.StateCompleted, Record: existing.body}, nil
	}
	return idempotency.Claim{State: idempotency.StateInProgress}, nil
}

func (s *memoryStore) Complete(_ context.Context, key string, rec idempotency.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry, ok := s.entries[key]; ok {
		entry.state = idempotency.StateCompleted
		entry.body = &rec
	}
	return nil
}

func (s *memoryStore) Release(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
	return nil
}

func (s *memoryStore) state(key string) (idempotency.State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return "", false
	}
	return entry.state, true
}

func testApp(store idempotency.Store, handler fiber.Handler) *fiber.App {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler(log)})
	app.Post("/bookings", idempotency.Middleware(store, log, time.Hour), handler)
	return app
}

func doRequest(t *testing.T, app *fiber.App, key, body string) (*http.Response, string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(idempotency.Header, key)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(payload)
}

func TestWithoutKeyHandlerRunsEveryTime(t *testing.T) {
	store := newMemoryStore()
	calls := 0
	app := testApp(store, func(c fiber.Ctx) error {
		calls++
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": 1})
	})

	for range 2 {
		if resp, _ := doRequest(t, app, "", `{"service_id":1}`); resp.StatusCode != fiber.StatusCreated {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	if calls != 2 {
		t.Errorf("handler called %d times, want 2", calls)
	}
}

func TestReplayReturnsStoredResponse(t *testing.T) {
	store := newMemoryStore()
	calls := 0
	app := testApp(store, func(c fiber.Ctx) error {
		calls++
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": 99})
	})

	first, firstBody := doRequest(t, app, "key-1", `{"service_id":1}`)
	if first.StatusCode != fiber.StatusCreated {
		t.Fatalf("first status = %d", first.StatusCode)
	}

	second, secondBody := doRequest(t, app, "key-1", `{"service_id":1}`)
	if second.StatusCode != fiber.StatusCreated {
		t.Fatalf("replay status = %d", second.StatusCode)
	}
	if firstBody != secondBody {
		t.Errorf("replayed body = %q, want %q", secondBody, firstBody)
	}
	if second.Header.Get("Idempotent-Replayed") != "true" {
		t.Error("replay must be marked with Idempotent-Replayed")
	}
	if calls != 1 {
		t.Errorf("handler called %d times, want 1", calls)
	}
}

func TestKeyReuseWithDifferentBodyConflicts(t *testing.T) {
	store := newMemoryStore()
	app := testApp(store, func(c fiber.Ctx) error {
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": 1})
	})

	doRequest(t, app, "key-2", `{"service_id":1}`)
	resp, body := doRequest(t, app, "key-2", `{"service_id":2}`)

	if resp.StatusCode != fiber.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if !strings.Contains(body, "idempotency_key_reuse") {
		t.Errorf("body = %s", body)
	}
}

func TestInProgressKeyConflicts(t *testing.T) {
	store := newMemoryStore()
	store.entries["key-3"] = &record{hash: httpx.RequestHash(http.MethodPost, "/bookings", []byte(`{"service_id":1}`)), state: idempotency.StateInProgress}
	app := testApp(store, func(c fiber.Ctx) error {
		t.Error("handler must not run for an in-progress key")
		return c.SendStatus(fiber.StatusCreated)
	})

	resp, body := doRequest(t, app, "key-3", `{"service_id":1}`)
	if resp.StatusCode != fiber.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if !strings.Contains(body, "request_in_progress") {
		t.Errorf("body = %s", body)
	}
}

func TestFailedRequestReleasesKey(t *testing.T) {
	store := newMemoryStore()
	app := testApp(store, func(c fiber.Ctx) error {
		return apperr.Unavailable("catalog_unavailable", "try again")
	})

	resp, _ := doRequest(t, app, "key-4", `{"service_id":1}`)
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if _, exists := store.state("key-4"); exists {
		t.Error("failed requests must release the idempotency key")
	}
}

func TestServerErrorIsNotCached(t *testing.T) {
	store := newMemoryStore()
	app := testApp(store, func(c fiber.Ctx) error {
		return c.Status(fiber.StatusInternalServerError).SendString("boom")
	})

	resp, _ := doRequest(t, app, "key-5", `{"service_id":1}`)
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if _, exists := store.state("key-5"); exists {
		t.Error("5xx responses must not be cached")
	}
}
