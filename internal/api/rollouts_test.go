package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/routing"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/worker"
)

// splitApplier also records canary splits (routing.RolloutApplier).
type splitApplier struct {
	recordingApplier
	splits map[string]*store.TrafficSplit
}

func (s *splitApplier) ApplyRollout(_ context.Context, app string, sp *store.TrafficSplit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.splits == nil {
		s.splits = map[string]*store.TrafficSplit{}
	}
	s.splits[app] = sp
	return nil
}

func (s *splitApplier) split(app string) *store.TrafficSplit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.splits[app]
}

func TestRolloutAPI(t *testing.T) {
	st := testdb.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ap := &splitApplier{}
	router := &routing.Syncer{Store: st, Applier: ap, Log: log}
	srv := httptest.NewServer((&api.Server{
		Store: st, Router: router, Domain: domain, APIToken: token, WebhookSecret: secret, Log: log,
	}).Handler())
	t.Cleanup(srv.Close)
	e := &env{t: t, srv: srv, wk: &worker.Worker{Store: st, Pipeline: worker.DryRunPipeline{}, Router: router, Domain: domain, Log: log}}
	e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, nil)
	e.push("nisagwn/blog", "main", sha(1))
	e.drain()

	var settings store.RolloutSettings
	if code := e.do("GET", "/api/apps/blog/rollout-settings", nil, &settings); code != 200 ||
		settings.Mode != store.RolloutInstant || store.FormatSteps(settings.Steps) != "10,50,100" || settings.StepSeconds != 300 {
		t.Fatalf("defaults: %d %+v", code, settings)
	}
	for _, bad := range []map[string]any{
		{"mode": "linear"},
		{"steps": []int{10, 90}},
		{"step_seconds": 60},
		{"max_p95_factor": 0.5},
		{"color": "blue"},
	} {
		var out map[string]any
		if code := e.do("PUT", "/api/apps/blog/rollout-settings", bad, &out); code != 400 {
			t.Errorf("%v: %d %v", bad, code, out)
		}
	}
	if code := e.do("PUT", "/api/apps/blog/rollout-settings",
		map[string]any{"mode": "canary", "steps": []int{20, 100}, "step_seconds": 120, "max_p95_ms": 800}, &settings); code != 200 ||
		settings.Mode != store.RolloutCanary || store.FormatSteps(settings.Steps) != "20,100" || settings.MaxP95Ms != 800 ||
		settings.MinRequests != 50 {
		t.Fatalf("put: %d %+v", code, settings)
	}

	// A new production deployment becomes a canary: production stays.
	_, d2 := e.push("nisagwn/blog", "main", sha(2))
	e.drain()
	if got := ap.route("blog", "blog.paas.test"); got != sha(1) {
		t.Fatalf("production moved during the canary: %s", got)
	}
	if got := ap.route("blog", "main-blog.paas.test"); got != sha(2) {
		t.Fatalf("branch preview: %s", got)
	}
	if sp := ap.split("blog"); sp == nil || sp.CanaryWeight != 20 || sp.Canary.CommitSHA != sha(2) || sp.Stable.CommitSHA != sha(1) {
		t.Fatalf("split: %+v", sp)
	}
	lines, _ := st.Logs(context.Background(), id(d2["id"]), 0, 100)
	var logText strings.Builder
	for _, l := range lines {
		logText.WriteString(l.Line + "\n")
	}
	if !strings.Contains(logText.String(), "==> canary: %20 trafik") || strings.Contains(logText.String(), "production alias") {
		t.Fatalf("deployment log:\n%s", logText.String())
	}

	var status api.RolloutStatus
	if code := e.do("GET", "/api/apps/blog/rollout", nil, &status); code != 200 || status.Active == nil ||
		status.Active.Weight != 20 || status.Active.Metrics == nil || status.Active.Metrics.Next != "hold" ||
		!strings.Contains(status.Active.ToURL, sha(2)[:7]) || len(status.Recent) != 1 {
		t.Fatalf("status: %d %+v", code, status)
	}

	act := func(action string, want int) store.Rollout {
		t.Helper()
		var r store.Rollout
		if code := e.do("POST", "/api/apps/blog/rollout/"+action, nil, &r); code != want {
			t.Fatalf("%s: %d, want %d", action, code, want)
		}
		return r
	}
	if r := act("pause", 200); r.State != store.RolloutPaused {
		t.Fatalf("pause: %+v", r)
	}
	act("pause", 409)
	act("resume", 200)
	act("explode", 404)
	if r := act("promote", 200); r.State != store.RolloutPromoted {
		t.Fatalf("promote: %+v", r)
	}
	if ap.route("blog", "blog.paas.test") != sha(2) || ap.split("blog") != nil {
		t.Fatalf("after promote: %s %+v", ap.route("blog", "blog.paas.test"), ap.split("blog"))
	}
	act("abort", 404)

	// The next canary is rolled back by hand: weight 0, production stays.
	e.push("nisagwn/blog", "main", sha(3))
	e.drain()
	if r := act("rollback", 200); r.State != store.RolloutRolledBack || r.Weight != 0 {
		t.Fatalf("rollback: %+v", r)
	}
	if ap.route("blog", "blog.paas.test") != sha(2) || ap.split("blog") != nil {
		t.Fatal("rollback changed production")
	}
	e.do("GET", "/api/apps/blog/rollout", nil, &status)
	if status.Active != nil || len(status.Recent) != 2 || status.Recent[0].State != store.RolloutRolledBack {
		t.Fatalf("recent: %+v", status)
	}

	// A manual rollback during a canary aborts it.
	e.push("nisagwn/blog", "main", sha(4))
	e.drain()
	if code := e.do("POST", "/api/apps/blog/rollback", map[string]any{"deployment_id": id(d2["id"])}, nil); code != 200 {
		t.Fatalf("rollback: %d", code)
	}
	e.do("GET", "/api/apps/blog/rollout", nil, &status)
	if status.Active != nil || status.Recent[0].State != store.RolloutAborted {
		t.Fatalf("after manual rollback: %+v", status.Recent[0])
	}
}
