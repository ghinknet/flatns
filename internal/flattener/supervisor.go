// Package flattener
// supervisor.go owns the lifecycle of all flatten workers. It builds one
// provider per configured alias, spawns a worker goroutine per flatten entry,
// and rebuilds everything on configuration reload. It is the single entry point
// the main package drives, keeping main itself trivial.
package flattener

import (
	"context"
	"sync"
	"time"

	"flatns/internal/infra/config"
	"flatns/internal/infra/logger"
	"flatns/internal/provider"
	"flatns/internal/resolver"

	"go.uber.org/zap"
)

// resolverTimeout bounds each individual DNS exchange performed by workers.
const resolverTimeout = 5 * time.Second

// Supervisor manages the set of running workers and rebuilds them on reload.
type Supervisor struct {
	parent context.Context

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// pkg-level singleton so the main package can drive it with package functions,
// matching the infra layer's Init/Cleanup ergonomics.
var sup Supervisor

// Start spawns the workers for the current configuration as children of ctx and
// registers a reload hook so the worker set is rebuilt whenever the config
// changes. Call Stop (via Cleanup) on shutdown.
func Start(ctx context.Context) {
	sup.parent = ctx
	sup.spawn()
	// Rebuild workers on every successful config reload.
	config.OnReload(sup.restart)
}

// Cleanup stops all workers and waits for them to exit.
func Cleanup() { sup.stop() }

// spawn builds providers and workers for the current config under a fresh child
// context.
func (s *Supervisor) spawn() {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := config.Get()
	ctx, cancel := context.WithCancel(s.parent)
	s.cancel = cancel

	// Build one provider instance per alias and share it across workers that
	// reference the same alias, avoiding redundant clients and rate-limit churn.
	providers := make(map[string]provider.Provider)
	for alias, pc := range cfg.Providers {
		p, err := provider.New(pc.ToProviderConfig())
		if err != nil {
			logger.L.Error("failed to build provider, entries using it will be skipped",
				zap.String("alias", alias), zap.Error(err))
			continue
		}
		providers[alias] = p
	}

	for _, fc := range cfg.Flattens {
		p, ok := providers[fc.Provider]
		if !ok {
			logger.L.Error("skipping flatten entry: provider unavailable",
				zap.String("flatten", fc.Name), zap.String("provider", fc.Provider))
			continue
		}
		res := resolver.New(fc.Resolvers, resolverTimeout)
		w := NewWorker(fc, p, res)

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			w.Run(ctx)
		}()
	}
}

// restart tears down the existing workers and spawns a fresh set from the
// latest configuration. Registered as a config reload hook.
func (s *Supervisor) restart() {
	logger.L.Info("applying new configuration")
	s.stop()
	s.spawn()
}

// stop cancels the current worker context and waits for completion.
func (s *Supervisor) stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}
