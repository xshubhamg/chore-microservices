package httpx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"go.opentelemetry.io/otel/trace"

	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/logging"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
)

const requestIDLocal = "request_id"

// RequestID propagates an incoming X-Request-ID or generates one. The value is
// returned to the client and attached to every log line for that request.
func RequestID() fiber.Handler {
	return func(c fiber.Ctx) error {
		id := c.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		c.Locals(requestIDLocal, id)
		c.Set("X-Request-ID", id)
		return c.Next()
	}
}

func RequestIDFrom(c fiber.Ctx) string {
	id, _ := c.Locals(requestIDLocal).(string)
	return id
}

// AccessLog emits one structured line per request with the fields needed to
// debug production incidents (request id, route, status, latency).
func AccessLog(log *slog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()
		err := c.Next()

		route := c.Route().Path
		if route == "" {
			route = "unmatched"
		}
		status := c.Response().StatusCode()
		if err != nil {
			status = observability.StatusForError(err)
		}

		attrs := []any{
			slog.String("request_id", RequestIDFrom(c)),
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.String("route", route),
			slog.Int("status", status),
			slog.Duration("duration", time.Since(start)),
			slog.String("ip", c.IP()),
			slog.Int("bytes", len(c.Response().Body())),
		}

		logger := logging.WithTrace(c.Context(), log).With(attrs...)
		switch {
		case err != nil || status >= 500:
			logger.Error("request failed", slog.String("error", errorString(err)))
		case status >= 400:
			logger.Warn("request rejected")
		default:
			logger.Info("request completed")
		}

		return err
	}
}

type errorBody struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Details   map[string]string `json:"details,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
	TraceID   string            `json:"trace_id,omitempty"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// ErrorHandler is the single place where errors become HTTP responses. Clients
// always receive the same envelope; unexpected errors never leak internals.
func ErrorHandler(log *slog.Logger) fiber.ErrorHandler {
	return func(c fiber.Ctx, err error) error {
		requestID := RequestIDFrom(c)
		traceID := ""
		if sc := trace.SpanContextFromContext(c.Context()); sc.IsValid() {
			traceID = sc.TraceID().String()
		}

		var fiberErr *fiber.Error
		if errors.As(err, &fiberErr) {
			body := errorBody{
				Code:      fiberCode(fiberErr.Code),
				Message:   fiberErr.Message,
				RequestID: requestID,
				TraceID:   traceID,
			}
			return c.Status(fiberErr.Code).JSON(errorEnvelope{Error: body})
		}

		appErr := apperr.From(err)
		logger := logging.WithTrace(c.Context(), log).With(
			slog.String("request_id", requestID),
			slog.String("code", appErr.Code),
		)
		if appErr.Kind == apperr.KindInternal {
			logger.Error("unhandled error", slog.String("error", appErr.Error()))
		} else {
			logger.Warn("request error", slog.String("error", appErr.Error()))
		}

		return c.Status(appErr.Kind.HTTPStatus()).JSON(errorEnvelope{Error: errorBody{
			Code:      appErr.Code,
			Message:   appErr.Message,
			Details:   appErr.Details,
			RequestID: requestID,
			TraceID:   traceID,
		}})
	}
}

func fiberCode(status int) string {
	switch status {
	case fiber.StatusNotFound:
		return "not_found"
	case fiber.StatusMethodNotAllowed:
		return "method_not_allowed"
	case fiber.StatusRequestEntityTooLarge:
		return "payload_too_large"
	case fiber.StatusTooManyRequests:
		return "rate_limited"
	case fiber.StatusServiceUnavailable:
		return "timeout"
	default:
		return "request_failed"
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// RequestHash is used by the idempotency middleware to detect reuse of a key
// with a different body.
func RequestHash(method, path string, body []byte) string {
	sum := sha256.Sum256(append([]byte(method+" "+path+"\n"), body...))
	return hex.EncodeToString(sum[:])
}
