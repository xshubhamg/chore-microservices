package idempotency

import (
	"errors"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
	"github.com/xshubhamg/chore-microservices/internal/platform/httpx"
	"github.com/xshubhamg/chore-microservices/internal/platform/logging"
)

const Header = "Idempotency-Key"

// Middleware implements the Idempotency-Key contract:
//
//	no key               -> pass through
//	key seen, completed  -> replay the stored response
//	key seen, in-flight  -> 409 (the client should retry later)
//	key seen, other body -> 409 (key reuse is a client bug)
//	first request        -> execute, store 2xx/4xx, release on 5xx
func Middleware(store Store, log *slog.Logger, ttl time.Duration) fiber.Handler {
	return func(c fiber.Ctx) error {
		key := c.Get(Header)
		if key == "" {
			return c.Next()
		}
		if len(key) > 255 {
			return apperr.Invalid("invalid_idempotency_key", "Idempotency-Key must be at most 255 characters")
		}

		ctx := c.Context()
		hash := httpx.RequestHash(c.Method(), c.Path(), c.Request().Body())

		claim, err := store.Claim(ctx, key, hash, ttl)
		if err != nil {
			if errors.Is(err, ErrKeyReuse) {
				return apperr.Conflict("idempotency_key_reuse", "Idempotency-Key was already used with a different request body")
			}
			return apperr.Internal(err)
		}

		switch claim.State {
		case StateCompleted:
			c.Set("Idempotent-Replayed", "true")
			c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
			return c.Status(claim.Record.StatusCode).Send(claim.Record.Body)

		case StateInProgress:
			return apperr.Conflict("request_in_progress", "a request with this Idempotency-Key is still being processed")
		}

		handlerErr := c.Next()

		if handlerErr != nil {
			if releaseErr := store.Release(ctx, key); releaseErr != nil {
				logging.WithTrace(ctx, log).Warn("release idempotency key failed",
					slog.String("key", key), slog.String("error", releaseErr.Error()))
			}
			return handlerErr
		}

		status := c.Response().StatusCode()
		if status >= 500 {
			// Do not cache server errors: the client should be able to retry.
			if releaseErr := store.Release(ctx, key); releaseErr != nil {
				logging.WithTrace(ctx, log).Warn("release idempotency key failed",
					slog.String("key", key), slog.String("error", releaseErr.Error()))
			}
			return nil
		}

		body := append([]byte(nil), c.Response().Body()...)
		if completeErr := store.Complete(ctx, key, Record{StatusCode: status, Body: body}); completeErr != nil {
			logging.WithTrace(ctx, log).Error("store idempotent response failed",
				slog.String("key", key), slog.String("error", completeErr.Error()))
		}
		return nil
	}
}
