package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/routing"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/worker"
)

// procCluster is the deployer side: runtime logs, process status and
// manual cron runs, plus the route and process applier of the syncer.
type procCluster struct {
	mu      sync.Mutex
	routes  []store.AliasRoute
	plan    process.Plan
	runErr  error
	runs    []string
	logProc string
}

func (c *procCluster) RuntimeLogs(_ context.Context, _ store.Deployment, _ bool, _ int64, w io.Writer) error {
	_, err := io.WriteString(w, "web logs\n")
	return err
}

func (c *procCluster) ProcessLogs(_ context.Context, _ store.Deployment, proc string, _ bool, _ int64, w io.Writer) error {
	c.mu.Lock()
	c.logProc = proc
	c.mu.Unlock()
	_, err := io.WriteString(w, proc+" logs\n")
	return err
}

func (c *procCluster) ProcessStatus(_ context.Context, d store.Deployment) (process.Status, error) {
	return process.Status{
		Web:     &process.ReplicaStatus{Desired: 1, Ready: 1, State: "running"},
		Workers: map[string]process.ReplicaStatus{"queue": {Desired: 2, Ready: 1, State: "starting"}},
		Crons:   map[string]process.CronStatus{"cleanup": {Exists: true}},
	}, nil
}

func (c *procCluster) RunCron(_ context.Context, d store.Deployment, cron string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runErr != nil {
		return "", c.runErr
	}
	c.runs = append(c.runs, cron)
	return "d-x-c-" + cron + "-m-1", nil
}

func (c *procCluster) ApplyAliases(_ context.Context, _ string, routes []store.AliasRoute) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.routes = routes
	return nil
}

func (c *procCluster) ApplyProcesses(_ context.Context, _ string, plan process.Plan) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plan = plan
	return nil
}

var workerSet = process.Set{
	Source:  "paas.yaml",
	Workers: []process.Worker{{Name: "queue", Command: "node q.js", Replicas: 2, Previews: true}},
	Crons:   []process.Cron{{Name: "cleanup", Schedule: "@daily", Command: "node c.js"}},
}

type procEnv struct {
	*env
	st      *store.Store
	cluster *procCluster
	app     store.App
}

func setupProcesses(t *testing.T) *procEnv {
	st := testdb.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &procCluster{}
	router := &routing.Syncer{Store: st, Applier: c, Log: log}
	srv := httptest.NewServer((&api.Server{
		Store: st, Router: router, Domain: domain, APIToken: token, WebhookSecret: secret, Log: log, RuntimeLogs: c,
	}).Handler())
	t.Cleanup(srv.Close)
	app, err := st.CreateApp(context.Background(), "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	return &procEnv{env: &env{t: t, srv: srv}, st: st, cluster: c, app: app}
}

// ready stores a ready deployment with a process set and its aliases.
func (e *procEnv) ready(n int, branch string, set process.Set) store.Deployment {
	e.t.Helper()
	ctx := context.Background()
	d, _, err := e.st.EnqueueDeployment(ctx, e.app.ID, sha(n), branch, "")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.SetProcesses(ctx, d.ID, set); err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.MarkReady(ctx, d, worker.Aliases(d, "main", domain)); err != nil {
		e.t.Fatal(err)
	}
	return d
}

func TestProcessesAPI(t *testing.T) {
	e := setupProcesses(t)
	if code := e.do("GET", "/api/apps/blog/processes", nil, nil); code != 404 {
		t.Fatalf("no production yet: %d", code)
	}
	d1 := e.ready(1, "main", workerSet)

	var v api.ProcessesView
	if code := e.do("GET", "/api/apps/blog/processes", nil, &v); code != 200 {
		t.Fatalf("get: %d", code)
	}
	if v.DeploymentID != d1.ID || !v.Production || v.Source != "paas.yaml" || len(v.Processes) != 2 || len(v.Crons) != 1 {
		t.Fatalf("view = %+v", v)
	}
	if web := v.Processes[0]; web.Name != "web" || web.State != "running" {
		t.Errorf("web = %+v", web)
	}
	if q := v.Processes[1]; q.Type != "worker" || q.Replicas != 2 || q.Desired != 2 || q.Ready != 1 || q.Override != nil || !q.Previews {
		t.Errorf("queue = %+v", q)
	}
	if c := v.Crons[0]; c.Name != "cleanup" || c.Schedule != "@daily" || !c.Exists {
		t.Errorf("cron = %+v", c)
	}

	// Scaling: an override for production, applied through the route sync.
	if code := e.do("PUT", "/api/apps/blog/processes/queue", map[string]any{"replicas": 4}, &v); code != 200 {
		t.Fatalf("scale: %d", code)
	}
	if q := v.Processes[1]; q.Override == nil || *q.Override != 4 {
		t.Errorf("override = %+v", q)
	}
	if got := e.cluster.plan.Replicas(d1.ID, "queue"); got != 4 {
		t.Errorf("applied replicas = %d, want 4", got)
	}
	for _, bad := range []struct {
		path string
		body any
		code int
	}{
		{"/api/apps/blog/processes/web", map[string]any{"replicas": 2}, 400},
		{"/api/apps/blog/processes/nope", map[string]any{"replicas": 2}, 404},
		{"/api/apps/blog/processes/queue", map[string]any{"replicas": 11}, 400},
		{"/api/apps/blog/processes/queue", map[string]any{"replicas": "2"}, 400},
		{"/api/apps/blog/processes/queue", map[string]any{}, 400},
		{"/api/apps/blog/processes/Bad_Name", map[string]any{"replicas": 1}, 400},
	} {
		if code := e.do("PUT", bad.path, bad.body, nil); code != bad.code {
			t.Errorf("PUT %s %v: %d, want %d", bad.path, bad.body, code, bad.code)
		}
	}
	var reset api.ProcessesView
	if code := e.do("PUT", "/api/apps/blog/processes/queue", map[string]any{"replicas": nil}, &reset); code != 200 ||
		reset.Processes[1].Override != nil {
		t.Errorf("reset: %d %+v", code, reset.Processes[1])
	}
	if got := e.cluster.plan.Replicas(d1.ID, "queue"); got != 2 {
		t.Errorf("after reset replicas = %d, want paas.yaml's 2", got)
	}

	// Manual cron runs.
	var run map[string]string
	if code := e.do("POST", "/api/apps/blog/crons/cleanup/run", nil, &run); code != 202 || run["job"] == "" {
		t.Fatalf("run: %d %v", code, run)
	}
	if code := e.do("POST", "/api/apps/blog/crons/nope/run", nil, nil); code != 404 {
		t.Errorf("unknown cron: %d", code)
	}
	e.cluster.runErr = deploy.ErrCronRunning
	if code := e.do("POST", "/api/apps/blog/crons/cleanup/run", nil, nil); code != 409 {
		t.Errorf("running cron: %d", code)
	}

	// Process logs.
	logs := func(q string) (int, string) {
		req, _ := http.NewRequest("GET", e.srv.URL+"/api/apps/blog/deployments/"+itoa(d1.ID)+"/runtime-logs"+q, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := logs("?process=queue"); code != 200 || !strings.Contains(body, "queue logs") {
		t.Errorf("queue logs: %d %q", code, body)
	}
	if code, body := logs("?process=cleanup"); code != 200 || !strings.Contains(body, "cleanup logs") {
		t.Errorf("cron logs: %d %q", code, body)
	}
	if code, body := logs("?process=web"); code != 200 || !strings.Contains(body, "web logs") {
		t.Errorf("web logs: %d %q", code, body)
	}
	if code, _ := logs("?process=nope"); code != 404 {
		t.Errorf("unknown process: %d", code)
	}
	if code, _ := logs("?process=Bad!"); code != 400 {
		t.Errorf("invalid process: %d", code)
	}
}

// A rollback moves the workers and crons back with the production alias;
// a deployment without web gets no Ingress but keeps its aliases.
func TestProcessesFollowRollback(t *testing.T) {
	e := setupProcesses(t)
	d1 := e.ready(1, "main", workerSet)
	d2 := e.ready(2, "main", process.Set{Source: "paas.yaml", NoWeb: true, Workers: workerSet.Workers, Crons: workerSet.Crons})
	feature := e.ready(3, "feature", workerSet)

	// d2 is production already; the rollback to it only syncs.
	if code := e.do("POST", "/api/apps/blog/rollback", map[string]any{"deployment_id": d2.ID}, nil); code != 200 {
		t.Fatalf("sync: %d", code)
	}
	p := e.cluster.plan
	if p.Replicas(d2.ID, "queue") != 2 || !p.CronActive(d2.ID, "cleanup") {
		t.Errorf("production (d2) does not run: %+v", p[d2.ID])
	}
	if p.Replicas(d1.ID, "queue") != 0 || p.CronActive(d1.ID, "cleanup") {
		t.Errorf("old deployment still runs: %+v", p[d1.ID])
	}
	if p.Replicas(feature.ID, "queue") != 2 || p.CronActive(feature.ID, "cleanup") {
		t.Errorf("preview runs previews-only processes: %+v", p[feature.ID])
	}
	for _, r := range e.cluster.routes {
		if r.DeploymentID == d2.ID {
			t.Errorf("route to the web-less deployment: %+v", r)
		}
	}

	if code := e.do("POST", "/api/apps/blog/rollback", map[string]any{"deployment_id": d1.ID}, nil); code != 200 {
		t.Fatalf("rollback: %d", code)
	}
	p = e.cluster.plan
	// d2 keeps the production branch's preview alias, but a production
	// deployment never runs as a preview.
	if p.Replicas(d1.ID, "queue") != 2 || !p.CronActive(d1.ID, "cleanup") || p.Replicas(d2.ID, "queue") != 0 ||
		p.CronActive(d2.ID, "cleanup") {
		t.Errorf("rollback did not move the processes: d1 %+v d2 %+v", p[d1.ID], p[d2.ID])
	}
	prod := false
	for _, r := range e.cluster.routes {
		if r.Kind == store.AliasProduction && r.DeploymentID == d1.ID {
			prod = true
		}
	}
	if !prod {
		t.Errorf("production route missing after rollback: %+v", e.cluster.routes)
	}

	// ?deployment= shows another deployment's processes.
	var v api.ProcessesView
	if code := e.do("GET", "/api/apps/blog/processes?deployment="+itoa(d2.ID), nil, &v); code != 200 ||
		v.Production || len(v.Processes) != 1 || v.Processes[0].Name != "queue" {
		t.Errorf("d2 view: %d %+v", code, v)
	}
	if code := e.do("GET", "/api/apps/blog/processes?deployment=x", nil, nil); code != 400 {
		t.Errorf("bad id: %d", code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
