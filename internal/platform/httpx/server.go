// Package httpx builds the HTTP server shared by every service: middleware
// stack, consistent error envelopes and graceful shutdown.
package httpx

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/helmet"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/timeout"

	"github.com/xshubhamg/chore-microservices/internal/platform/config"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
)

type Server struct {
	app  *fiber.App
	addr string
	log  *slog.Logger
}

// NewServer assembles the middleware stack. Order matters:
//  1. recover            - a panic must never kill the process
//  2. request id         - every log line and error response can be correlated
//  3. access log         - logs the final status, including errors
//  4. tracing/metrics    - observe everything, including failed requests
//  5. helmet/timeout     - cheap hardening
func NewServer(cfg config.Config, log *slog.Logger, metrics *observability.Metrics) *Server {
	app := fiber.New(fiber.Config{
		AppName:      cfg.Service,
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout:  cfg.HTTP.IdleTimeout,
		BodyLimit:    cfg.HTTP.BodyLimit,
		ErrorHandler: ErrorHandler(log),
	})

	app.Use(recover.New(recover.Config{
		EnableStackTrace: cfg.Env == config.EnvDevelopment,
	}))
	app.Use(RequestID())
	app.Use(AccessLog(log))
	app.Use(observability.HTTPTracing(cfg.Service))
	app.Use(observability.HTTPMetrics(metrics))
	app.Use(helmet.New())
	app.Use(timeout.New(func(c fiber.Ctx) error { return c.Next() }, timeout.Config{Timeout: cfg.HTTP.RequestTimeout}))

	return &Server{app: app, addr: cfg.HTTP.Addr, log: log}
}

// App exposes the underlying Fiber app so route packages can register
// handlers (and tests can call app.Test).
func (s *Server) App() *fiber.App { return s.app }

// Start blocks until the server stops. A graceful shutdown is reported as nil.
func (s *Server) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}

	s.log.Info("http server listening", slog.String("addr", listener.Addr().String()))

	if err := s.app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
		return err
	}
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	return s.app.ShutdownWithContext(ctx)
}

// RateLimit returns a middleware enforcing HTTP_RATE_LIMIT_PER_MINUTE. A value
// of zero disables limiting, which keeps tests hermetic.
func RateLimit(cfg config.HTTPConfig) fiber.Handler {
	if cfg.RateLimitPerMinute <= 0 {
		return func(c fiber.Ctx) error { return c.Next() }
	}
	return limiter.New(limiter.Config{
		Max:        cfg.RateLimitPerMinute,
		Expiration: time.Minute,
	})
}
