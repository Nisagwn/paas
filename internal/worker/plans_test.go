package worker_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/worker"
)

// slowPipeline builds for build (or until its context ends).
type slowPipeline struct{ build time.Duration }

func (p slowPipeline) Build(ctx context.Context, d store.Deployment, _ store.BuildSettings, _ worker.Logger) (worker.BuildResult, error) {
	select {
	case <-time.After(p.build):
		return worker.BuildResult{Image: "reg/" + d.AppName + "@sha256:x"}, nil
	case <-ctx.Done():
		return worker.BuildResult{}, ctx.Err()
	}
}

func (slowPipeline) Deploy(context.Context, store.Deployment, string, worker.Logger) error {
	return nil
}

// timeoutPlans is a worker.Plans with a short build timeout.
type timeoutPlans struct {
	*plans.Guard
	timeout time.Duration
}

func (p timeoutPlans) BuildTimeout(context.Context, int64) (time.Duration, error) {
	return p.timeout, nil
}

// The plan's build timeout stops the build; the time it ran is recorded.
func TestPlanBuildTimeout(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("a", 40), "main", "")
	w := newWorker(st, slowPipeline{build: time.Minute})
	w.Plans = timeoutPlans{Guard: &plans.Guard{Store: st}, timeout: 1200 * time.Millisecond}
	start := time.Now()
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("timeout not applied")
	}
	got, _ := st.GetDeployment(ctx, d.ID)
	if got.Status != store.StatusFailed || !strings.Contains(got.Error, "build timeout of 1.2s") {
		t.Fatalf("status=%s error=%q", got.Status, got.Error)
	}
	if secs, _ := st.BuildSeconds(ctx, d.ID); secs != 1 {
		t.Fatalf("build seconds %d", secs)
	}
}

// Every build records its duration for the monthly build minutes.
func TestBuildSecondsRecorded(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	team, _ := st.CreateTeam(ctx, "web", "Web", 0)
	app, _ := st.CreateAppInTeam(ctx, team.ID, "blog", "o/blog", "main")
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("b", 40), "main", "")
	w := newWorker(st, slowPipeline{build: 1600 * time.Millisecond})
	g := &plans.Guard{Store: st}
	w.Plans = g
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDeployment(ctx, d.ID)
	if got.Status != store.StatusReady {
		t.Fatalf("status %s: %s", got.Status, got.Error)
	}
	if secs, _ := st.BuildSeconds(ctx, d.ID); secs != 2 {
		t.Fatalf("build seconds %d", secs)
	}
	if s, _ := g.Status(ctx, team.ID); s.BuildMinutes != 1 || s.Usage.BuildSeconds != 2 {
		t.Fatalf("usage %+v", s.Usage)
	}
}

// A paas.yaml asking for more than the plan allows fails the deployment.
func TestPlanProcessLimits(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	team, _ := st.CreateTeam(ctx, "web", "Web", 0)
	app, _ := st.CreateAppInTeam(ctx, team.ID, "blog", "o/blog", "main")
	cat := plans.DefaultCatalog()
	h := cat[store.PlanHobby]
	h.Crons, h.Replicas = 1, 2
	cat[store.PlanHobby] = h
	for i, tc := range []struct {
		set  process.Set
		want string
	}{
		{process.Set{Crons: []process.Cron{{Name: "a", Schedule: "* * * * *", Command: "x"}, {Name: "b", Schedule: "* * * * *", Command: "y"}}},
			"the Hobby plan allows 1 cron jobs; paas.yaml declares 2"},
		{process.Set{Workers: []process.Worker{{Name: "queue", Command: "q", Replicas: 3}}},
			"paas.yaml asks for 3 replicas of queue"},
		{process.Set{Workers: []process.Worker{{Name: "queue", Command: "q", Replicas: 2}}}, ""},
	} {
		d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat(string(rune('c'+i)), 40), "main", "")
		w := newWorker(st, &processPipeline{set: tc.set})
		w.Plans = &plans.Guard{Store: st, Catalog: cat}
		if _, err := w.ProcessOne(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetDeployment(ctx, d.ID)
		if tc.want == "" {
			if got.Status != store.StatusReady {
				t.Fatalf("case %d: %s %s", i, got.Status, got.Error)
			}
			continue
		}
		if got.Status != store.StatusFailed || !strings.Contains(got.Error, tc.want) {
			t.Fatalf("case %d: status=%s error=%q", i, got.Status, got.Error)
		}
	}
}

// The worker's claim follows the plans: a team at its bound of concurrent
// builds waits while another team's deployment is built.
func TestWorkerFairQueue(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	busy, _ := st.CreateTeam(ctx, "busy", "Busy", 0)
	calm, _ := st.CreateTeam(ctx, "calm", "Calm", 0)
	a, _ := st.CreateAppInTeam(ctx, busy.ID, "app-a", "o/app-a", "main")
	b, _ := st.CreateAppInTeam(ctx, calm.ID, "app-b", "o/app-b", "main")
	running, _, _ := st.EnqueueDeployment(ctx, a.ID, strings.Repeat("1", 40), "main", "")
	st.SetStatus(ctx, running.ID, store.StatusBuilding) // another worker builds it
	waiting, _, _ := st.EnqueueDeployment(ctx, a.ID, strings.Repeat("2", 40), "main", "")
	other, _, _ := st.EnqueueDeployment(ctx, b.ID, strings.Repeat("3", 40), "main", "")

	w := newWorker(st, worker.DryRunPipeline{})
	w.Plans = &plans.Guard{Store: st} // hobby: one build at a time
	if worked, err := w.ProcessOne(ctx); err != nil || !worked {
		t.Fatalf("process: %v %v", worked, err)
	}
	if got, _ := st.GetDeployment(ctx, other.ID); got.Status != store.StatusReady {
		t.Fatalf("other team's deployment: %s", got.Status)
	}
	if got, _ := st.GetDeployment(ctx, waiting.ID); got.Status != store.StatusQueued {
		t.Fatalf("busy team's second deployment: %s", got.Status)
	}
	if worked, _ := w.ProcessOne(ctx); worked {
		t.Fatal("claimed beyond the team's bound")
	}
	// Without plans the queue is strictly in order.
	w.Plans = nil
	if worked, _ := w.ProcessOne(ctx); !worked {
		t.Fatal("unbounded worker idle")
	}
	if got, _ := st.GetDeployment(ctx, waiting.ID); got.Status != store.StatusReady {
		t.Fatalf("unbounded: %s", got.Status)
	}
}
