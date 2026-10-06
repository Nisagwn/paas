package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func TestRolloutSettingsValidate(t *testing.T) {
	ok := store.DefaultRolloutSettings()
	if err := ok.Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	for name, mut := range map[string]func(*store.RolloutSettings){
		"mode":            func(s *store.RolloutSettings) { s.Mode = "blue-green" },
		"one step":        func(s *store.RolloutSettings) { s.Steps = []int{100} },
		"not ending 100":  func(s *store.RolloutSettings) { s.Steps = []int{10, 50} },
		"decreasing":      func(s *store.RolloutSettings) { s.Steps = []int{50, 10, 100} },
		"zero":            func(s *store.RolloutSettings) { s.Steps = []int{0, 100} },
		"duplicate":       func(s *store.RolloutSettings) { s.Steps = []int{10, 10, 100} },
		"too many":        func(s *store.RolloutSettings) { s.Steps = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 100} },
		"short step":      func(s *store.RolloutSettings) { s.StepSeconds = 60 },
		"short guard":     func(s *store.RolloutSettings) { s.GuardSeconds = 30 },
		"error pct":       func(s *store.RolloutSettings) { s.MaxErrorPct = 0 },
		"error increase":  func(s *store.RolloutSettings) { s.MaxErrorIncreasePct = -1 },
		"p95 ms":          func(s *store.RolloutSettings) { s.MaxP95Ms = -5 },
		"p95 factor":      func(s *store.RolloutSettings) { s.MaxP95Factor = 0.5 },
		"min requests":    func(s *store.RolloutSettings) { s.MinRequests = 0 },
		"error pct > 100": func(s *store.RolloutSettings) { s.MaxErrorPct = 101 },
	} {
		s := store.DefaultRolloutSettings()
		mut(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, s)
		}
	}
	steps, err := store.ParseSteps(" 5, 25%,100")
	if err != nil || store.FormatSteps(steps) != "5,25,100" {
		t.Fatalf("parse: %v %v", steps, err)
	}
	if _, err := store.ParseSteps("10,x"); err == nil {
		t.Fatal("bad steps parsed")
	}
}

// rolloutFixture is an app with a ready production deployment (#1).
type rolloutFixture struct {
	t   *testing.T
	st  *store.Store
	app store.App
	n   int
}

func newRolloutFixture(t *testing.T, mode string) (*rolloutFixture, store.Deployment) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	f := &rolloutFixture{t: t, st: st, app: app}
	first, ro := f.ready("main")
	if ro != nil {
		t.Fatal("first production deployment started a rollout")
	}
	if mode != store.RolloutInstant {
		s := store.DefaultRolloutSettings()
		s.Mode = mode
		if _, err := st.SetRolloutSettings(ctx, app.ID, s); err != nil {
			t.Fatal(err)
		}
	}
	return f, first
}

// queue enqueues and claims the next commit on branch.
func (f *rolloutFixture) queue(branch string) store.Deployment {
	f.t.Helper()
	ctx := context.Background()
	f.n++
	if _, _, err := f.st.EnqueueDeployment(ctx, f.app.ID, sha(f.n), branch, ""); err != nil {
		f.t.Fatal(err)
	}
	d, err := f.st.ClaimNext(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *rolloutFixture) markReady(d store.Deployment) *store.Rollout {
	f.t.Helper()
	aliases := []store.AliasSpec{{Hostname: d.Branch + "-blog.paas.test", Kind: store.AliasPreview, Branch: d.Branch}}
	if d.Target == store.EnvProduction {
		aliases = append(aliases, store.AliasSpec{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: d.Branch})
	}
	ro, err := f.st.MarkReadyWithRollout(context.Background(), d, aliases)
	if err != nil {
		f.t.Fatal(err)
	}
	return ro
}

func (f *rolloutFixture) ready(branch string) (store.Deployment, *store.Rollout) {
	d := f.queue(branch)
	return d, f.markReady(d)
}

func (f *rolloutFixture) production() int64 {
	f.t.Helper()
	aliases, err := f.st.ListAliases(context.Background(), f.app.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, a := range aliases {
		if a.Kind == store.AliasProduction {
			return a.DeploymentID
		}
	}
	return 0
}

func (f *rolloutFixture) preview(branch string) int64 {
	aliases, _ := f.st.ListAliases(context.Background(), f.app.ID)
	for _, a := range aliases {
		if a.Kind == store.AliasPreview && a.Branch == branch {
			return a.DeploymentID
		}
	}
	return 0
}

func TestCanaryKeepsProductionAlias(t *testing.T) {
	f, first := newRolloutFixture(t, store.RolloutCanary)
	ctx := context.Background()

	// Previews never start a rollout.
	if _, ro := f.ready("feature"); ro != nil {
		t.Fatalf("preview rollout: %+v", ro)
	}
	second, ro := f.ready("main")
	if ro == nil || ro.Mode != store.RolloutCanary || ro.Weight != 10 || ro.State != store.RolloutRunning ||
		ro.ToDeploymentID != second.ID || *ro.FromDeploymentID != first.ID || len(ro.Verdicts) != 1 {
		t.Fatalf("rollout: %+v", ro)
	}
	if got := f.production(); got != first.ID {
		t.Fatalf("production moved to %d during the canary", got)
	}
	if got := f.preview("main"); got != second.ID {
		t.Fatalf("branch preview alias = %d, want the canary %d", got, second.ID)
	}

	// The split covers the production alias and routed custom domains.
	if _, err := f.st.AddDomain(ctx, f.app.ID, "www.example.com", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Exec(ctx, `UPDATE app_domains SET routed = true`); err != nil {
		t.Fatal(err)
	}
	sp, err := f.st.TrafficSplit(ctx, f.app.ID)
	if err != nil || sp == nil {
		t.Fatalf("split: %v %v", sp, err)
	}
	if sp.Stable.DeploymentID != first.ID || sp.Canary.DeploymentID != second.ID || sp.CanaryWeight != 10 ||
		len(sp.Routes) != 2 || sp.Routes[0].Hostname != "blog.paas.test" || sp.Routes[1].Kind != store.AliasCustom {
		t.Fatalf("split = %+v", sp)
	}

	// Both sides are protected from scale to zero and from retirement.
	cands, err := f.st.ScaleCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		want := c.DeploymentID == first.ID || c.DeploymentID == second.ID
		if c.InRollout != want {
			t.Errorf("candidate %d: in rollout %v", c.DeploymentID, c.InRollout)
		}
	}
	if err := f.st.Exec(ctx, `DELETE FROM aliases WHERE deployment_id = $1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if retired, err := f.st.Retire(ctx, second.ID, "test"); err != nil || retired {
		t.Fatalf("canary retired: %v %v", retired, err)
	}

	// Promotion moves the production alias; the split ends.
	got, err := f.st.ManualRolloutAction(ctx, f.app.ID, store.ActionPromote, "alice")
	if err != nil || got.State != store.RolloutPromoted || got.Weight != 100 || got.FinishedAt == nil {
		t.Fatalf("promote: %+v %v", got, err)
	}
	if f.production() != second.ID {
		t.Fatal("production alias did not move")
	}
	if sp, _ := f.st.TrafficSplit(ctx, f.app.ID); sp != nil {
		t.Fatalf("split after promotion: %+v", sp)
	}
	// Manual rollback still works afterwards (the alias row is the same).
	if _, err := f.st.Rollback(ctx, f.app.ID, first.ID); err != nil || f.production() != first.ID {
		t.Fatalf("rollback after promotion: %v", err)
	}
}

func TestCanarySupersedeAndLateBuild(t *testing.T) {
	f, first := newRolloutFixture(t, store.RolloutCanary)
	ctx := context.Background()
	older := f.queue("main") // #2 builds slowly
	second, ro1 := f.ready("main")
	if ro1 == nil {
		t.Fatal("no rollout")
	}
	// The older build finishing during the canary does not take production.
	if ro := f.markReady(older); ro != nil {
		t.Fatalf("late build started a rollout: %+v", ro)
	}
	if f.production() != first.ID {
		t.Fatalf("late build took production: %d", f.production())
	}
	// A newer production deployment supersedes the canary and starts from
	// the stable production.
	third, ro2 := f.ready("main")
	if ro2 == nil || *ro2.FromDeploymentID != first.ID || ro2.ToDeploymentID != third.ID {
		t.Fatalf("second rollout: %+v", ro2)
	}
	old, err := f.st.GetRollout(ctx, ro1.ID)
	if err != nil || old.State != store.RolloutSuperseded || old.Weight != 0 || !strings.Contains(old.Reason, "#") {
		t.Fatalf("superseded: %+v %v", old, err)
	}
	if sp, _ := f.st.TrafficSplit(ctx, f.app.ID); sp == nil || sp.Canary.DeploymentID != third.ID {
		t.Fatalf("split: %+v", sp)
	}
	list, _ := f.st.ListRollouts(ctx, f.app.ID, 10)
	if len(list) != 2 || list[0].ID != ro2.ID {
		t.Fatalf("list: %+v", list)
	}
	_ = second

	// A manual rollback aborts the canary.
	if _, err := f.st.Rollback(ctx, f.app.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.st.GetRollout(ctx, ro2.ID); r.State != store.RolloutAborted || r.Weight != 0 {
		t.Fatalf("after manual rollback: %+v", r)
	}
	if _, err := f.st.ActiveRollout(ctx, f.app.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("active after abort: %v", err)
	}
}

func TestPromotionSkipsCanary(t *testing.T) {
	f, _ := newRolloutFixture(t, store.RolloutCanary)
	ctx := context.Background()
	_, ro := f.ready("main")
	if ro == nil {
		t.Fatal("no canary")
	}
	feat, _ := f.ready("feature")
	feat, _ = f.st.GetDeployment(ctx, feat.ID)
	copyD, err := f.st.CopyDeployment(ctx, feat, store.CopyOptions{Origin: store.OriginPromote, Target: store.EnvProduction, ReuseImage: true})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.st.ClaimNext(ctx)
	if err != nil || claimed.ID != copyD.ID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if got := f.markReady(claimed); got != nil {
		t.Fatalf("promotion started a canary: %+v", got)
	}
	if f.production() != copyD.ID {
		t.Fatal("promotion did not move production at once")
	}
	if r, _ := f.st.GetRollout(ctx, ro.ID); r.State != store.RolloutAborted {
		t.Fatalf("canary not aborted by the promotion: %+v", r)
	}
}

func TestGuardedAndManualActions(t *testing.T) {
	f, first := newRolloutFixture(t, store.RolloutGuarded)
	ctx := context.Background()
	second, ro := f.ready("main")
	if ro == nil || ro.Mode != store.RolloutGuarded || ro.Weight != 100 {
		t.Fatalf("guard: %+v", ro)
	}
	if f.production() != second.ID {
		t.Fatal("guarded mode must switch at once")
	}
	if sp, _ := f.st.TrafficSplit(ctx, f.app.ID); sp != nil {
		t.Fatalf("guard has no split: %+v", sp)
	}

	// pause / resume / invalid transitions.
	if _, err := f.st.ManualRolloutAction(ctx, f.app.ID, store.ActionResume, "bob"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("resume running: %v", err)
	}
	p, err := f.st.ManualRolloutAction(ctx, f.app.ID, store.ActionPause, "bob")
	if err != nil || p.State != store.RolloutPaused || !strings.Contains(p.Reason, "bob") {
		t.Fatalf("pause: %+v %v", p, err)
	}
	if _, err := f.st.ManualRolloutAction(ctx, f.app.ID, store.ActionPause, "bob"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("pause paused: %v", err)
	}
	r, err := f.st.ManualRolloutAction(ctx, f.app.ID, store.ActionResume, "bob")
	if err != nil || r.State != store.RolloutRunning || !r.StepStartedAt.After(ro.StepStartedAt) {
		t.Fatalf("resume: %+v %v", r, err)
	}
	if _, err := f.st.ManualRolloutAction(ctx, f.app.ID, "explode", "bob"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("unknown action: %v", err)
	}
	// Rolling a guard back moves production back.
	rb, err := f.st.ManualRolloutAction(ctx, f.app.ID, store.ActionRollback, "bob")
	if err != nil || rb.State != store.RolloutRolledBack || f.production() != first.ID {
		t.Fatalf("guard rollback: %+v %v (production %d)", rb, err, f.production())
	}
	if len(rb.Verdicts) != 4 {
		t.Fatalf("verdicts: %+v", rb.Verdicts)
	}
	if _, err := f.st.ManualRolloutAction(ctx, f.app.ID, store.ActionAbort, "bob"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no active rollout: %v", err)
	}

	// Compare and set: a stale view is refused.
	_, ro3 := f.ready("main")
	if _, err := f.st.UpdateRollout(ctx, *ro3, store.RolloutChange{State: store.RolloutPaused}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpdateRollout(ctx, *ro3, store.RolloutChange{State: store.RolloutPromoted}); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale update: %v", err)
	}
}

func TestInstantModeAndClaim(t *testing.T) {
	f, _ := newRolloutFixture(t, store.RolloutInstant)
	ctx := context.Background()
	second, ro := f.ready("main")
	if ro != nil || f.production() != second.ID {
		t.Fatalf("instant: %+v, production %d", ro, f.production())
	}
	s, _ := f.st.GetRolloutSettings(ctx, f.app.ID)
	s.Mode = store.RolloutCanary
	if _, err := f.st.SetRolloutSettings(ctx, f.app.ID, s); err != nil {
		t.Fatal(err)
	}
	s.StepSeconds = 10
	if _, err := f.st.SetRolloutSettings(ctx, f.app.ID, s); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("invalid settings stored: %v", err)
	}
	_, ro = f.ready("main")
	due, err := f.st.ClaimDueRollouts(ctx, time.Minute)
	if err != nil || len(due) != 1 || due[0].ID != ro.ID || due[0].Settings.Mode != store.RolloutCanary {
		t.Fatalf("claim: %+v %v", due, err)
	}
	// Claimed: not due again within the period.
	if due, _ := f.st.ClaimDueRollouts(ctx, time.Minute); len(due) != 0 {
		t.Fatalf("claimed twice: %+v", due)
	}
	// Switching back to instant: the next production deployment supersedes.
	s.Mode, s.StepSeconds = store.RolloutInstant, 300
	f.st.SetRolloutSettings(ctx, f.app.ID, s)
	fourth, ro4 := f.ready("main")
	if ro4 != nil || f.production() != fourth.ID {
		t.Fatalf("instant again: %+v", ro4)
	}
	if r, _ := f.st.GetRollout(ctx, ro.ID); r.State != store.RolloutSuperseded {
		t.Fatalf("not superseded: %+v", r)
	}
}

func TestWindowTraffic(t *testing.T) {
	f, first := newRolloutFixture(t, store.RolloutInstant)
	ctx := context.Background()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	err := f.st.AddRequestMetrics(ctx, []store.RequestBucket{
		{DeploymentID: first.ID, Minute: base, Requests: 10, Classes: [4]int64{9, 0, 0, 1},
			DurationCount: 10, Buckets: map[float64]float64{0.1: 9, 1: 10}},
		{DeploymentID: first.ID, Minute: base.Add(time.Minute), Requests: 5, Classes: [4]int64{5, 0, 0, 0},
			DurationCount: 5, Buckets: map[float64]float64{0.1: 5, 1: 5}},
		{DeploymentID: first.ID, Minute: base.Add(2 * time.Minute), Requests: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.st.WindowTraffic(ctx, first.ID, base, base.Add(2*time.Minute))
	if err != nil || p.Requests != 15 || p.Classes[3] != 1 || p.Buckets[0.1] != 14 || p.Buckets[1] != 15 {
		t.Fatalf("window: %+v %v", p, err)
	}
	if p, _ := f.st.WindowTraffic(ctx, first.ID, base.Add(time.Hour), base.Add(2*time.Hour)); p.Requests != 0 {
		t.Fatalf("empty window: %+v", p)
	}
}
