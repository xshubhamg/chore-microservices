// Package admin serves operational endpoints on a separate port so probes and
// metrics are never exposed through the public API listener.
package admin

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	"github.com/xshubhamg/chore-microservices/internal/platform/config"
	"github.com/xshubhamg/chore-microservices/internal/platform/health"
	"github.com/xshubhamg/chore-microservices/internal/platform/observability"
)

type Server struct {
	app  *fiber.App
	addr string
	log  *slog.Logger
}

func New(cfg config.Config, registry *health.Registry, metrics *observability.Metrics, log *slog.Logger) *Server {
	app := fiber.New(fiber.Config{
		AppName:      cfg.Service + "-admin",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	})

	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status":  "ok",
			"service": cfg.Service,
			"version": cfg.Version,
		})
	})

	app.Get("/readyz", func(c fiber.Ctx) error {
		ready, checks := registry.Ready(c.Context())
		if !ready {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"status": "unavailable",
				"checks": checks,
			})
		}
		return c.JSON(fiber.Map{
			"status": "ready",
			"checks": checks,
		})
	})

	app.Get("/metrics", adaptor.HTTPHandler(metrics.Handler()))

	app.Get("/version", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"service": cfg.Service,
			"version": cfg.Version,
			"env":     cfg.Env,
		})
	})

	return &Server{app: app, addr: cfg.Admin.Addr, log: log}
}

func (s *Server) Start(context.Context) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}

	s.log.Info("admin server listening", slog.String("addr", listener.Addr().String()))

	if err := s.app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
		return err
	}
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	return s.app.ShutdownWithContext(ctx)
}
