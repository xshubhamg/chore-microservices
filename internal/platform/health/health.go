// Package health aggregates liveness and readiness checks. Liveness must never
// depend on a downstream system: a slow database should fail readiness, not
// restart the pod.
package health

import (
	"context"
	"sync"
	"time"
)

type Check struct {
	Name string
	Fn   func(context.Context) error
}

type Registry struct {
	checks []Check
}

func New() *Registry { return &Registry{} }

func (r *Registry) Add(name string, fn func(context.Context) error) {
	r.checks = append(r.checks, Check{Name: name, Fn: fn})
}

// Ready runs every check concurrently with a short per-check timeout. The
// returned map is suitable for a /readyz JSON body.
func (r *Registry) Ready(ctx context.Context) (bool, map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = make(map[string]string, len(r.checks))
		ready   = true
	)

	for _, check := range r.checks {
		wg.Add(1)
		go func(check Check) {
			defer wg.Done()

			checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			status := "ok"
			if err := check.Fn(checkCtx); err != nil {
				status = err.Error()
				mu.Lock()
				ready = false
				mu.Unlock()
			}

			mu.Lock()
			results[check.Name] = status
			mu.Unlock()
		}(check)
	}

	wg.Wait()
	return ready, results
}
