package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/routing"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/worker"
)

// recordingApplier stands in for the ingress layer: it remembers, per app,
// which commit each hostname was last routed to.
type recordingApplier struct {
	mu     sync.Mutex
	routes map[string]map[string]string // app → hostname → commit
	err    error
}

func (r *recordingApplier) ApplyAliases(_ context.Context, app string, routes []store.AliasRoute) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	m := map[string]string{}
	for _, rt := range routes {
		m[rt.Hostname] = rt.CommitSHA
	}
	if r.routes == nil {
		r.routes = map[string]map[string]string{}
	}
	r.routes[app] = m
	return nil
}

func (r *recordingApplier) route(app, host string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.routes[app][host]
}

func setupRouted(t *testing.T) (*env, *recordingApplier, *routing.Syncer) {
	st := testdb.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ap := &recordingApplier{}
	router := &routing.Syncer{Store: st, Applier: ap, Log: log}
	srv := httptest.NewServer((&api.Server{
		Store: st, Router: router, Domain: domain, APIToken: token, WebhookSecret: secret, Log: log,
	}).Handler())
	t.Cleanup(srv.Close)
	wk := &worker.Worker{Store: st, Pipeline: worker.DryRunPipeline{}, Router: router, Domain: domain, Log: log}
	return &env{t: t, srv: srv, wk: wk}, ap, router
}

// The ingress layer follows the database: new deploys move aliases forward,
// a rollback moves production back at once, and nothing is rebuilt.
func TestRoutesFollowDeploysAndRollback(t *testing.T) {
	e, ap, _ := setupRouted(t)
	e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, nil)

	_, d1 := e.push("nisagwn/blog", "main", sha(1))
	e.drain()
	if got := ap.route("blog", "blog.paas.test"); got != sha(1) {
		t.Fatalf("after first deploy production → %q, want %s", got, sha(1))
	}

	e.push("nisagwn/blog", "main", sha(2))
	e.push("nisagwn/blog", "feature/x", sha(3))
	e.drain()
	if ap.route("blog", "blog.paas.test") != sha(2) || ap.route("blog", "feature-x-blog.paas.test") != sha(3) {
		t.Fatalf("routes after second deploy: %v", ap.routes)
	}

	if code := e.do("POST", "/api/apps/blog/rollback", map[string]any{"deployment_id": d1["id"]}, nil); code != 200 {
		t.Fatalf("rollback: %d", code)
	}
	if got := ap.route("blog", "blog.paas.test"); got != sha(1) {
		t.Fatalf("after rollback production → %q, want %s", got, sha(1))
	}
	// The preview alias of main still follows the latest main deploy.
	if got := ap.route("blog", "main-blog.paas.test"); got != sha(2) {
		t.Fatalf("main preview → %q, want %s", got, sha(2))
	}
}

// A router failure must not lose the rollback: the database keeps it, the
// client hears about it, and the next reconcile applies it.
func TestRollbackRouterFailure(t *testing.T) {
	e, ap, router := setupRouted(t)
	e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, nil)
	_, d1 := e.push("nisagwn/blog", "main", sha(1))
	e.push("nisagwn/blog", "main", sha(2))
	e.drain()

	ap.err = errors.New("ingress API unavailable")
	var out map[string]any
	if code := e.do("POST", "/api/apps/blog/rollback", map[string]any{"deployment_id": d1["id"]}, &out); code != 502 {
		t.Fatalf("rollback with failing router: %d %v, want 502", code, out)
	}
	if e.aliases()["blog.paas.test"] != d1["id"] {
		t.Fatal("rollback must be saved even when routing fails")
	}

	ap.err = nil
	if failed := router.SyncAll(context.Background()); failed != 0 {
		t.Fatalf("reconcile: %d app(s) failed", failed)
	}
	if got := ap.route("blog", "blog.paas.test"); got != sha(1) {
		t.Fatalf("after reconcile production → %q, want %s", got, sha(1))
	}
}

// A failed alias sync after a deploy is a warning, not a failed deployment:
// the deployment's own URL works.
func TestDeployRouterFailureIsWarning(t *testing.T) {
	e, ap, _ := setupRouted(t)
	ap.err = errors.New("ingress API unavailable")
	e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, nil)
	_, d := e.push("nisagwn/blog", "main", sha(1))
	e.drain()

	var got map[string]any
	e.do("GET", fmt.Sprintf("/api/deployments/%v", d["id"]), nil, &got)
	if got["status"] != "ready" {
		t.Fatalf("status = %v, want ready", got["status"])
	}
	var logs []map[string]any
	e.do("GET", fmt.Sprintf("/api/deployments/%v/logs", d["id"]), nil, &logs)
	found := false
	for _, l := range logs {
		if s, _ := l["line"].(string); strings.HasPrefix(s, "WARNING: updating alias routes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no routing warning in logs: %v", logs)
	}
}
