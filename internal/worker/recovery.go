package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

// Retirer removes a deployment's objects from the cluster (Faz 7). It must
// be idempotent: deleting what is already gone succeeds.
type Retirer interface {
	Retire(ctx context.Context, d store.Deployment) error
}

// Retire implements Retirer: a dry run created nothing.
func (DryRunPipeline) Retire(context.Context, store.Deployment) error { return nil }

// Kicker is told that an app may have something to clean up, e.g. a failed
// deployment's leftovers or an old deployment beyond the retention policy
// (cleanup.Collector). It must not block.
type Kicker interface {
	Kick(app string)
}

// DefaultMaxAttempts: a deployment orphaned by a dead worker is retried once.
const DefaultMaxAttempts = 2

// errLeaseLost cancels a run whose deployment was taken over: it was
// recovered as orphaned (heartbeat too old) and queued again.
var errLeaseLost = errors.New("deployment was taken over after a missed heartbeat")

// errCanceled cancels a run whose deployment a user canceled (Faz 17).
var errCanceled = errors.New("deployment canceled by a user")

func (w *Worker) maxAttempts() int {
	if w.MaxAttempts > 0 {
		return w.MaxAttempts
	}
	return DefaultMaxAttempts
}

func (w *Worker) kick(app string) {
	if w.Cleanup != nil {
		w.Cleanup.Kick(app)
	}
}

// startHeartbeat refreshes the deployment's heartbeat every StaleAfter/4
// (every CancelPoll without heartbeats) until the returned stop is called.
// If the claim was lost, it cancels the run with errLeaseLost so two workers
// never keep running one deployment; if a user canceled the deployment, it
// cancels the run with errCanceled.
func (w *Worker) startHeartbeat(ctx context.Context, d store.Deployment, cancel context.CancelCauseFunc) (stop func()) {
	interval := w.CancelPoll
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if w.StaleAfter > 0 {
		interval = max(w.StaleAfter/4, 10*time.Millisecond)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			st, err := w.Store.Beat(ctx, d.ID, d.Attempts)
			if err != nil {
				// A database blip; the next beat retries. Only a heartbeat
				// missing for StaleAfter makes the deployment an orphan.
				w.Log.Warn("heartbeat", "deployment", d.ID, "err", err)
				continue
			}
			if !st.Owned {
				cancel(errLeaseLost)
				return
			}
			if st.CancelRequested {
				cancel(errCanceled)
				return
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// RecoverStale takes back deployments whose worker stopped sending
// heartbeats for StaleAfter (process crash, OOM kill, node loss): each is
// queued again until it has been claimed MaxAttempts times, then failed.
func (w *Worker) RecoverStale(ctx context.Context) ([]store.Recovered, error) {
	if w.StaleAfter <= 0 {
		return nil, nil
	}
	limit := w.maxAttempts()
	reason := fmt.Sprintf("worker restarted: no heartbeat for %s during the deployment; gave up after %d attempt(s)",
		w.StaleAfter, limit)
	recovered, err := w.Store.RecoverStale(ctx, w.StaleAfter, limit, reason)
	if err != nil {
		return nil, err
	}
	for _, r := range recovered {
		switch r.Status {
		case store.StatusQueued:
			w.logLine(ctx, r.ID, "==> worker stopped responding (no heartbeat for %s); queued again (attempt %d of %d done)",
				w.StaleAfter, r.Attempts, limit)
		case store.StatusFailed:
			w.logLine(ctx, r.ID, "ERROR: %s", reason)
		case store.StatusCanceled:
			w.logLine(ctx, r.ID, "==> worker stopped responding; the deployment had been canceled")
		default:
			w.logLine(ctx, r.ID, "==> worker stopped responding; the branch was deleted meanwhile, deployment retired")
		}
		w.Log.Warn("recovered orphaned deployment", "deployment", r.ID, "app", r.AppName,
			"status", r.Status, "attempts", r.Attempts)
		if r.Status != store.StatusQueued {
			w.kick(r.AppName) // partially created objects
		}
	}
	return recovered, nil
}

// recoverLoop runs RecoverStale at start and every StaleAfter/2.
func (w *Worker) recoverLoop(ctx context.Context) {
	t := time.NewTicker(max(w.StaleAfter/2, 10*time.Millisecond))
	defer t.Stop()
	for {
		if _, err := w.RecoverStale(ctx); err != nil && ctx.Err() == nil {
			w.Log.Error("recover stale deployments", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
