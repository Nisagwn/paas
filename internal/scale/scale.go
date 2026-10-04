// Package scale puts idle deployments to sleep and wakes them on the first
// request (Faz 11).
//
// Idle detection: every Interval the Scaler reads Traefik's per-service
// request counters. A deployment whose counter has not changed for
// IdleAfter is idle. Any change (including a reset after a Traefik restart)
// counts as activity, and a failed read puts nothing to sleep, so errors
// only ever keep deployments awake.
//
// Sleeping (deploy.Kubernetes.Sleep) points the deployment's Service at the
// Activator and scales it to zero. The Activator receives the first request,
// wakes the deployment, holds the request until a pod is ready and then
// proxies it. Production deployments only sleep when the app opts in
// (apps.scale_to_zero_production); a sleeping deployment that becomes
// production (rollback) or loses its opt-in is woken by the Scaler.
package scale

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// Cluster is the Kubernetes side (deploy.Kubernetes).
type Cluster interface {
	Workloads(ctx context.Context) ([]deploy.Workload, error)
	Sleep(ctx context.Context, ns, name string) error
	Wake(ctx context.Context, ns, name string) error
	RepointActivator(ctx context.Context) error
	Backend(ctx context.Context, host string) (deploy.Workload, error)
	PodAddr(ctx context.Context, ns, name string) (string, error)
}

// Counter returns request totals keyed by deploy.Workload.MetricKey
// (deploy.Kubernetes.RequestCounts).
type Counter interface {
	RequestCounts(ctx context.Context) (map[string]float64, error)
}

// Store is the database side (store.Store).
type Store interface {
	ScaleCandidates(ctx context.Context) ([]store.ScaleCandidate, error)
	SetSleeping(ctx context.Context, id int64, sleeping bool) error
}

type Scaler struct {
	Cluster Cluster
	Counter Counter
	Store   Store
	// IdleAfter without requests puts a deployment to sleep.
	IdleAfter time.Duration
	// Interval of the idle check. Zero means min(IdleAfter/4, 30s).
	Interval time.Duration
	Log      *slog.Logger

	now     func() time.Time
	started time.Time

	mu    sync.Mutex
	seen  map[string]*activity // by workload key
	locks map[string]*sync.Mutex
	wakes map[string]*wake
}

type activity struct {
	count  float64
	active time.Time // last observed change, or the baseline
	// noCount: active is known but the counter has not been read yet.
	noCount bool
}

// wake is one in-flight Wake shared by all requests for the deployment.
type wake struct {
	done chan struct{}
	err  error
}

func (s *Scaler) init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen != nil {
		return
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.started = s.now()
	s.seen, s.locks, s.wakes = map[string]*activity{}, map[string]*sync.Mutex{}, map[string]*wake{}
}

func (s *Scaler) lock(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	return l
}

// touch records activity now, e.g. a request that reached the activator.
func (s *Scaler) touch(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.seen[key]; ok {
		a.active = s.now()
	} else {
		s.seen[key] = &activity{active: s.now(), noCount: true}
	}
}

// Run checks at start and every Interval until ctx ends.
func (s *Scaler) Run(ctx context.Context) {
	s.init()
	interval := s.Interval
	if interval <= 0 {
		interval = min(max(s.IdleAfter/4, time.Second), 30*time.Second)
	}
	// After a restart the activator may have a new address.
	if err := s.Cluster.RepointActivator(ctx); err != nil {
		s.Log.Error("scale: repoint activator", "err", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Result counts what one check did.
type Result struct {
	Slept, Woken, Errors int
}

// Check runs one idle check over all ready deployments.
func (s *Scaler) Check(ctx context.Context) Result {
	s.init()
	var res Result
	workloads, err := s.Cluster.Workloads(ctx)
	if err != nil {
		s.Log.Error("scale: list workloads", "err", err)
		res.Errors++
		return res
	}
	byKey := make(map[string]deploy.Workload, len(workloads))
	for _, w := range workloads {
		byKey[w.Key()] = w
	}
	cands, err := s.Store.ScaleCandidates(ctx)
	if err != nil {
		s.Log.Error("scale: candidates", "err", err)
		res.Errors++
		return res
	}
	counts, cerr := s.Counter.RequestCounts(ctx)
	if cerr != nil {
		// Without counters idleness is unknown: nothing goes to sleep.
		s.Log.Warn("scale: request counters unavailable; not scaling down", "err", cerr)
	}

	now := s.now()
	for _, c := range cands {
		w, ok := byKey[naming.Namespace(c.AppName)+"/"+naming.ResourceName(c.CommitSHA)]
		if !ok || w.DeploymentID != c.DeploymentID {
			continue // not deployed (yet), or a different commit with the same prefix
		}
		eligible := !c.Production || c.ScaleProduction

		// Mirror the cluster state into the database.
		if w.Sleeping != (c.SleepingSince != nil) {
			if err := s.Store.SetSleeping(ctx, c.DeploymentID, w.Sleeping); err != nil {
				s.Log.Error("scale: record state", "deployment", c.DeploymentID, "err", err)
			}
		}

		if w.Sleeping {
			// Production without opt-in (after a rollback or a setting
			// change), or a wake interrupted by a restart: wake it.
			if !eligible || w.Replicas > 0 {
				go func() {
					if err := s.Wake(context.WithoutCancel(ctx), w); err != nil {
						s.Log.Error("scale: wake", "deployment", w.Key(), "err", err)
					}
				}()
				res.Woken++
			}
			continue
		}
		if cerr != nil || !eligible || s.IdleAfter <= 0 {
			continue
		}
		if !s.observe(w, counts[w.MetricKey], c.FinishedAt, now) {
			continue
		}
		if err := s.Sleep(ctx, w); err != nil {
			s.Log.Error("scale: sleep", "deployment", w.Key(), "err", err)
			res.Errors++
			continue
		}
		res.Slept++
	}
	return res
}

// observe updates the activity of w and reports whether it is idle. The
// first observation starts from the later of the scaler's start and the
// deployment's finish time: history before a restart is unknown.
func (s *Scaler) observe(w deploy.Workload, count float64, finished, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.seen[w.Key()]
	switch {
	case !ok:
		base := s.started
		if finished.After(base) {
			base = finished
		}
		a = &activity{count: count, active: base}
		s.seen[w.Key()] = a
	case a.noCount:
		a.count, a.noCount = count, false
	case count != a.count:
		a.count, a.active = count, now
	}
	return now.Sub(a.active) >= s.IdleAfter
}

// Sleep scales w to zero unless it became active meanwhile.
func (s *Scaler) Sleep(ctx context.Context, w deploy.Workload) error {
	l := s.lock(w.Key())
	l.Lock()
	defer l.Unlock()
	s.mu.Lock()
	a := s.seen[w.Key()]
	idle := a == nil || s.now().Sub(a.active) >= s.IdleAfter
	s.mu.Unlock()
	if !idle {
		return nil // woken by a request since the check
	}
	if err := s.Cluster.Sleep(ctx, w.Namespace, w.Name); err != nil {
		return err
	}
	s.Log.Info("scaled to zero", "deployment", w.Key(), "id", w.DeploymentID, "idle_after", s.IdleAfter)
	if err := s.Store.SetSleeping(ctx, w.DeploymentID, true); err != nil {
		s.Log.Error("scale: record sleeping", "deployment", w.DeploymentID, "err", err)
	}
	return nil
}

// Wake brings w back. Concurrent calls for the same deployment share one
// wake; a caller whose ctx ends stops waiting but the wake goes on.
func (s *Scaler) Wake(ctx context.Context, w deploy.Workload) error {
	s.init()
	key := w.Key()
	s.mu.Lock()
	wk, running := s.wakes[key]
	if !running {
		wk = &wake{done: make(chan struct{})}
		s.wakes[key] = wk
	}
	s.mu.Unlock()

	if !running {
		go func() {
			wk.err = s.wake(w)
			s.mu.Lock()
			delete(s.wakes, key)
			s.mu.Unlock()
			close(wk.done)
		}()
	}
	select {
	case <-wk.done:
		return wk.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scaler) wake(w deploy.Workload) error {
	ctx := context.Background() // bounded by the cluster's rollout timeout
	l := s.lock(w.Key())
	l.Lock()
	defer l.Unlock()
	start := s.now()
	s.touch(w.Key())
	if err := s.Cluster.Wake(ctx, w.Namespace, w.Name); err != nil {
		return err
	}
	s.touch(w.Key()) // idle time counts from the wake
	if w.Sleeping {
		s.Log.Info("woke up", "deployment", w.Key(), "id", w.DeploymentID,
			"ms", s.now().Sub(start).Milliseconds())
	}
	if err := s.Store.SetSleeping(ctx, w.DeploymentID, false); err != nil {
		s.Log.Error("scale: record awake", "deployment", w.DeploymentID, "err", err)
	}
	return nil
}

// errNoRoute is answered with 404 by the activator.
var errNoRoute = errors.New("no deployment serves this host")

func backend(ctx context.Context, c Cluster, host string) (deploy.Workload, error) {
	w, err := c.Backend(ctx, host)
	if errors.Is(err, deploy.ErrNotFound) {
		return w, fmt.Errorf("%w: %s", errNoRoute, host)
	}
	return w, err
}
