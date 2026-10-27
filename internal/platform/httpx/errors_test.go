package httpx_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/httpx"
)

type errorEnvelope struct {
	Error struct {
		Code      string            `json:"code"`
		Message   string            `json:"message"`
		Details   map[string]string `json:"details"`
		RequestID string            `json:"request_id"`
	} `json:"error"`
}

func testApp(handler fiber.Handler) *fiber.App {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler(log)})
	app.Use(httpx.RequestID())
	app.Get("/boom", handler)
	return app
}

func decode(t *testing.T, resp *http.Response) errorEnvelope {
	t.Helper()
	var envelope errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return envelope
}

func TestApplicationErrorEnvelope(t *testing.T) {
	app := testApp(func(fiber.Ctx) error {
		return apperr.Invalid("invalid_booking", "booking details are invalid").
			WithDetail("customer_email", "must be valid")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/boom", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	envelope := decode(t, resp)
	if envelope.Error.Code != "invalid_booking" {
		t.Errorf("code = %q", envelope.Error.Code)
	}
	if envelope.Error.Details["customer_email"] == "" {
		t.Error("details should be preserved")
	}
	if envelope.Error.RequestID == "" {
		t.Error("request_id should be returned")
	}
	if got := resp.Header.Get("X-Request-ID"); got != envelope.Error.RequestID {
		t.Errorf("header request id = %q, body = %q", got, envelope.Error.RequestID)
	}
}

func TestInternalErrorDoesNotLeakCause(t *testing.T) {
	app := testApp(func(fiber.Ctx) error {
		return apperr.Internal(io.ErrUnexpectedEOF)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/boom", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	envelope := decode(t, resp)
	if envelope.Error.Message != "an unexpected error occurred" {
		t.Errorf("message = %q, leaks internals", envelope.Error.Message)
	}
}

func TestNotFoundUsesSameEnvelope(t *testing.T) {
	app := testApp(func(c fiber.Ctx) error { return c.Next() })

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/missing", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if envelope := decode(t, resp); envelope.Error.Code != "not_found" {
		t.Errorf("code = %q", envelope.Error.Code)
	}
}
