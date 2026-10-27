// Package run is a tiny supervision tree. Every service registers actors that
// start a server or worker plus the hook that stops it. Run cancels everything
// on the first failure or on SIGINT/SIGTERM, then drains in reverse order.
package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"log/slog"
)

type actor struct {
	name      string
	execute   func(context.Context) error
	interrupt func(context.Context) error
}

type Group struct {
	actors          []actor
	shutdownTimeout time.Duration
	log             *slog.Logger
}

func New(log *slog.Logger, shutdownTimeout time.Duration) *Group {
	return &Group{log: log, shutdownTimeout: shutdownTimeout}
}

// Add registers an actor. execute blocks until the actor stops (usually a
// server) and interrupt performs a graceful stop. interrupt is called with a
// context bounded by the group's shutdown timeout.
func (g *Group) Add(name string, execute func(context.Context) error, interrupt func(context.Context) error) {
	g.actors = append(g.actors, actor{name: name, execute: execute, interrupt: interrupt})
}

// Run blocks until an actor fails, an actor returns cleanly, or a termination
// signal arrives. It returns the first error encountered.
func (g *Group) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan error, len(g.actors))
	var wg sync.WaitGroup

	for _, a := range g.actors {
		wg.Add(1)
		go func(a actor) {
			defer wg.Done()
			err := a.execute(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				results <- fmt.Errorf("%s: %w", a.name, err)
				return
			}
			results <- nil
		}(a)
	}

	var firstErr error
	select {
	case <-ctx.Done():
		g.log.Info("shutdown signal received")
	case err := <-results:
		if err != nil {
			firstErr = err
			g.log.Error("actor failed, shutting down", slog.String("error", err.Error()))
		}
	}

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), g.shutdownTimeout)
	defer shutdownCancel()

	for i := len(g.actors) - 1; i >= 0; i-- {
		a := g.actors[i]
		g.log.Info("stopping actor", slog.String("actor", a.name))
		if err := a.interrupt(shutdownCtx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("stop %s: %w", a.name, err)
		}
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-shutdownCtx.Done():
		if firstErr == nil {
			firstErr = fmt.Errorf("shutdown timed out after %s", g.shutdownTimeout)
		}
	}

	return firstErr
}
