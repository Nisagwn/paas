// Package routing keeps the cluster's alias routes in line with the
// database. Postgres is the source of truth for where each alias points
// (MarkReady moves aliases forward, Rollback moves production back); this
// package pushes that state to the ingress layer:
//
//   - right after a deployment becomes ready (worker)
//   - right after a rollback (API)
//   - periodically, to repair drift or a sync that failed earlier
package routing

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

// Applier makes the ingress layer match an app's aliases (deploy.Kubernetes).
type Applier interface {
	ApplyAliases(ctx context.Context, app string, routes []store.AliasRoute) error
}

type Syncer struct {
	Store   *store.Store
	Applier Applier
	// Interval of the reconcile loop in Run. Zero means one minute.
	Interval time.Duration
	Log      *slog.Logger
	// Rollouts applies canary traffic splits (Faz 21); nil uses Applier
	// when it implements RolloutApplier.
	Rollouts RolloutApplier

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// SyncApp reads the app's aliases and applies them. Syncs of one app are
// serialized and each reads the database inside the lock, so the last sync
// to finish always applies the latest state: a slow sync started before a
// rollback cannot undo it.
func (s *Syncer) SyncApp(ctx context.Context, appName string) error {
	l := s.lock(appName)
	l.Lock()
	defer l.Unlock()

	app, err := s.Store.GetAppByName(ctx, appName)
	if err != nil {
		return err
	}
	routes, err := s.Store.AliasRoutes(ctx, app.ID)
	if err != nil {
		return err
	}
	// Faz 20 (processes.go): no Ingress for deployments without web, and
	// workers and crons follow the aliases.
	if routes, err = s.webRoutes(ctx, app.ID, routes); err != nil {
		return err
	}
	if err := s.Applier.ApplyAliases(ctx, app.Name, routes); err != nil {
		return err
	}
	// Both run even if the other fails: a failed process sync must not
	// leave a canary split stale, and the reverse.
	return errors.Join(s.applyProcesses(ctx, app), s.applyRollout(ctx, app))
}

// RolloutApplier splits an app's production hostnames between two
// deployments during a canary (Faz 21, deploy.Kubernetes.ApplyRollout);
// a nil split removes the split.
type RolloutApplier interface {
	ApplyRollout(ctx context.Context, app string, split *store.TrafficSplit) error
}

// applyRollout runs after ApplyAliases, so a split never outlives the
// alias it overrides: the alias Ingress is up to date before the overlay
// is added or removed. Rollouts defaults to the Applier when it can split.
func (s *Syncer) applyRollout(ctx context.Context, app store.App) error {
	ra := s.Rollouts
	if ra == nil {
		ra, _ = s.Applier.(RolloutApplier)
	}
	if ra == nil {
		return nil
	}
	split, err := s.Store.TrafficSplit(ctx, app.ID)
	if err != nil {
		return err
	}
	return ra.ApplyRollout(ctx, app.Name, split)
}

func (s *Syncer) lock(app string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	l, ok := s.locks[app]
	if !ok {
		l = &sync.Mutex{}
		s.locks[app] = l
	}
	return l
}

// SyncAll reconciles every app and returns how many failed.
func (s *Syncer) SyncAll(ctx context.Context) int {
	apps, err := s.Store.ListApps(ctx)
	if err != nil {
		s.Log.Error("route sync: list apps", "err", err)
		return 1
	}
	failed := 0
	for _, a := range apps {
		if err := s.SyncApp(ctx, a.Name); err != nil {
			failed++
			s.Log.Error("route sync", "app", a.Name, "err", err)
		}
	}
	return failed
}

// Run reconciles all apps at start and then every Interval until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.SyncAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
