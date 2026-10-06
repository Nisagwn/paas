package rollout_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/rollout"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

type fakeRouter struct {
	mu    sync.Mutex
	syncs int
	err   error
}

func (r *fakeRouter) SyncApp(ctx context.Context, app string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncs++
	return r.err
}

type fakeNotifier struct{ got []store.Rollout }

func (n *fakeNotifier) RolloutFinished(ctx context.Context, d store.Deployment, r store.Rollout) error {
	if d.ID != r.ToDeploymentID {
		return fmt.Errorf("deployment %d for rollout of %d", d.ID, r.ToDeploymentID)
	}
	n.got = append(n.got, r)
	return nil
}

type fixture struct {
	t        *testing.T
	st       *store.Store
	app      store.App
	n        int
	router   *fakeRouter
	notifier *fakeNotifier
	c        *rollout.Controller
	now      time.Time
}

func setup(t *testing.T, mode string) (*fixture, store.Deployment) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, st: st, app: app, router: &fakeRouter{}, notifier: &fakeNotifier{}}
	f.c = &rollout.Controller{Store: st, Router: f.router, Notifier: f.notifier, Now: func() time.Time { return f.now }}
	first, _ := f.ready()
	s := store.DefaultRolloutSettings()
	s.Mode, s.StepSeconds, s.GuardSeconds, s.MinRequests = mode, 120, 180, 10
	if _, err := st.SetRolloutSettings(ctx, app.ID, s); err != nil {
		t.Fatal(err)
	}
	return f, first
}

func (f *fixture) ready() (store.Deployment, *store.Rollout) {
	f.t.Helper()
	ctx := context.Background()
	f.n++
	if _, _, err := f.st.EnqueueDeployment(ctx, f.app.ID, fmt.Sprintf("%040x", f.n), "main", ""); err != nil {
		f.t.Fatal(err)
	}
	d, err := f.st.ClaimNext(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	ro, err := f.st.MarkReadyWithRollout(ctx, d, []store.AliasSpec{
		{Hostname: "main-blog.paas.test", Kind: store.AliasPreview, Branch: "main"},
		{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return d, ro
}

// traffic stores one minute of requests for d, offset minutes after t.
func (f *fixture) traffic(d int64, t time.Time, offset int, req, errs int64, p95 float64) {
	f.t.Helper()
	b := store.RequestBucket{
		DeploymentID: d, Minute: t.UTC().Truncate(time.Minute).Add(time.Duration(offset) * time.Minute),
		Requests: req, Classes: [4]int64{req - errs, 0, 0, errs}, DurationCount: req,
		// Everything under p95 (seconds): the estimate lands in that bucket.
		Buckets: map[float64]float64{p95 / 2: 0, p95: float64(req)},
	}
	if err := f.st.AddRequestMetrics(context.Background(), []store.RequestBucket{b}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) production() int64 {
	aliases, _ := f.st.ListAliases(context.Background(), f.app.ID)
	for _, a := range aliases {
		if a.Kind == store.AliasProduction {
			return a.DeploymentID
		}
	}
	return 0
}

func (f *fixture) evaluate(r store.Rollout) store.Rollout {
	f.t.Helper()
	if _, err := f.c.Evaluate(context.Background(), r); err != nil {
		f.t.Fatal(err)
	}
	cur, err := f.st.GetRollout(context.Background(), r.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return cur
}

func (f *fixture) logs(id int64) string {
	lines, _ := f.st.Logs(context.Background(), id, 0, 100)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Line + "\n")
	}
	return b.String()
}

func TestCanaryAdvancesAndPromotes(t *testing.T) {
	f, first := setup(t, store.RolloutCanary)
	second, ro := f.ready()
	r := *ro

	// One minute in: holding, no decision yet.
	f.now = r.StepStartedAt.Add(time.Minute)
	if r = f.evaluate(r); r.Step != 0 || r.Weight != 10 || !strings.Contains(r.Reason, "adım sürüyor") {
		t.Fatalf("hold: %+v", r)
	}
	// Healthy traffic on both sides over the step: advance to 50 %.
	f.traffic(second.ID, r.StepStartedAt, 1, 30, 0, 0.1)
	f.traffic(first.ID, r.StepStartedAt, 1, 300, 1, 0.1)
	f.now = r.StepStartedAt.Add(3 * time.Minute)
	r = f.evaluate(r)
	if r.Step != 1 || r.Weight != 50 || r.State != store.RolloutRunning {
		t.Fatalf("advance: %+v", r)
	}
	last := r.Verdicts[len(r.Verdicts)-1]
	if last.Action != "advance" || last.Canary.Requests != 30 || last.Baseline.Requests != 300 || *last.Canary.P95Ms != 97.5 {
		t.Fatalf("verdict: %+v", last)
	}
	if !strings.Contains(f.logs(second.ID), "==> canary: %50 trafik") || f.router.syncs < 2 {
		t.Fatalf("log %q, syncs %d", f.logs(second.ID), f.router.syncs)
	}
	if f.production() != first.ID {
		t.Fatal("production moved before 100 %")
	}

	// The last step passes: production moves to the canary.
	f.traffic(second.ID, r.StepStartedAt, 1, 100, 0, 0.1)
	f.now = r.StepStartedAt.Add(3 * time.Minute)
	r = f.evaluate(r)
	if r.State != store.RolloutPromoted || r.Weight != 100 || f.production() != second.ID {
		t.Fatalf("promote: %+v, production %d", r, f.production())
	}
	if len(f.notifier.got) != 1 || f.notifier.got[0].State != store.RolloutPromoted {
		t.Fatalf("notifications: %+v", f.notifier.got)
	}
	// A finished rollout is not claimed again.
	if due, _ := f.st.ClaimDueRollouts(context.Background(), 0); len(due) != 0 {
		t.Fatalf("due: %+v", due)
	}
}

func TestCanaryRollsBackOn5xx(t *testing.T) {
	f, first := setup(t, store.RolloutCanary)
	second, ro := f.ready()
	f.traffic(second.ID, ro.StepStartedAt, 1, 40, 12, 0.1)
	f.traffic(first.ID, ro.StepStartedAt, 1, 400, 0, 0.1)
	// Fail fast: well before the step ends.
	f.now = ro.StepStartedAt.Add(2*time.Minute + 5*time.Second)
	if n := f.c.Tick(context.Background()); n != 1 {
		t.Fatalf("tick decided %d", n)
	}
	r, _ := f.st.GetRollout(context.Background(), ro.ID)
	if r.State != store.RolloutRolledBack || r.Weight != 0 || !strings.Contains(r.Reason, "5xx oranı %30.0") {
		t.Fatalf("rollback: %+v", r)
	}
	if f.production() != first.ID {
		t.Fatal("production moved")
	}
	d, _ := f.st.GetDeployment(context.Background(), second.ID)
	if d.Status != store.StatusReady {
		t.Fatalf("the rolled back deployment stays ready on its own URL: %s", d.Status)
	}
	if !strings.Contains(f.logs(second.ID), "canary geri alındı") || len(f.notifier.got) != 1 ||
		f.notifier.got[0].State != store.RolloutRolledBack {
		t.Fatalf("log %q, notifications %+v", f.logs(second.ID), f.notifier.got)
	}
}

func TestGuardRollsBackProduction(t *testing.T) {
	f, first := setup(t, store.RolloutGuarded)
	second, ro := f.ready()
	if f.production() != second.ID {
		t.Fatal("guarded mode switches at once")
	}
	// Before the switch production was healthy; after it, it fails.
	f.traffic(first.ID, ro.CreatedAt, -2, 200, 0, 0.05)
	f.traffic(second.ID, ro.StepStartedAt, 1, 100, 50, 0.05)
	f.now = ro.StepStartedAt.Add(2*time.Minute + time.Second)
	r := f.evaluate(*ro)
	if r.State != store.RolloutRolledBack || f.production() != first.ID {
		t.Fatalf("guard rollback: %+v, production %d", r, f.production())
	}
	if !strings.Contains(f.logs(second.ID), "otomatik geri alındı") {
		t.Fatalf("log: %q", f.logs(second.ID))
	}
}

func TestGuardPassesAndMissingMetrics(t *testing.T) {
	f, first := setup(t, store.RolloutGuarded)
	second, ro := f.ready()
	f.now = ro.StepStartedAt.Add(4 * time.Minute)
	if r := f.evaluate(*ro); r.State != store.RolloutPromoted || f.production() != second.ID {
		t.Fatalf("idle guard ends well: %+v", r)
	}

	// Production had traffic before the switch but nothing is measured
	// since: hold, then abort without touching production.
	third, ro3 := f.ready()
	f.traffic(second.ID, ro3.CreatedAt, -1, 500, 0, 0.05)
	f.now = ro3.StepStartedAt.Add(4 * time.Minute)
	r := f.evaluate(*ro3)
	if r.State != store.RolloutRunning || !strings.Contains(r.Reason, "metriği yok") {
		t.Fatalf("hold on missing metrics: %+v", r)
	}
	f.now = ro3.StepStartedAt.Add(10 * time.Minute)
	if r = f.evaluate(r); r.State != store.RolloutAborted || f.production() != third.ID {
		t.Fatalf("abort: %+v", r)
	}
	_ = first
}

func TestCanaryWithoutTraefikCRDs(t *testing.T) {
	f, first := setup(t, store.RolloutCanary)
	_, ro := f.ready()
	f.router.err = fmt.Errorf("traefikservice: %w", deploy.ErrTraefikCRD)
	f.now = ro.StepStartedAt.Add(10 * time.Minute)
	r := f.evaluate(*ro)
	if r.State != store.RolloutAborted || r.Weight != 0 || !strings.Contains(r.Reason, "trafik bölünemiyor") {
		t.Fatalf("abort: %+v", r)
	}
	if f.production() != first.ID {
		t.Fatal("production moved")
	}

	// Any other routing error holds the rollout instead of judging it.
	f.router.err = nil
	_, ro2 := f.ready()
	f.router.err = fmt.Errorf("api server unavailable")
	f.now = ro2.StepStartedAt.Add(10 * time.Minute)
	if _, err := f.c.Evaluate(context.Background(), *ro2); err == nil {
		t.Fatal("routing error not reported")
	}
	if r, _ := f.st.GetRollout(context.Background(), ro2.ID); r.State != store.RolloutRunning || r.Step != 0 {
		t.Fatalf("held: %+v", r)
	}
}

func TestStaleDecisionDropped(t *testing.T) {
	f, _ := setup(t, store.RolloutCanary)
	_, ro := f.ready()
	// A user pauses while the controller holds an old view.
	if _, err := f.st.ManualRolloutAction(context.Background(), f.app.ID, store.ActionPause, "bob"); err != nil {
		t.Fatal(err)
	}
	f.now = ro.StepStartedAt.Add(10 * time.Minute)
	acted, err := f.c.Evaluate(context.Background(), *ro)
	if err != nil || acted {
		t.Fatalf("stale decision applied: %v %v", acted, err)
	}
	if r, _ := f.st.GetRollout(context.Background(), ro.ID); r.State != store.RolloutPaused {
		t.Fatalf("state: %+v", r)
	}
}
