package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/webhook"
	"github.com/nisagwn/paas/internal/worker"
)

const (
	token  = "test-token"
	secret = "test-secret"
	domain = "paas.test"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	wk  *worker.Worker
}

func setup(t *testing.T, p worker.Pipeline) *env {
	st := testdb.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer((&api.Server{
		Store: st, Domain: domain, APIToken: token, WebhookSecret: secret, Log: log,
	}).Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, wk: &worker.Worker{Store: st, Pipeline: p, Domain: domain, Log: log}}
}

func (e *env) do(method, path string, body any, out any) int {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	return e.send(req, out)
}

func (e *env) push(repo, branch, sha string) (int, map[string]any) {
	e.t.Helper()
	body := []byte(fmt.Sprintf(`{"ref":"refs/heads/%s","after":"%s","repository":{"full_name":"%s"},
		"head_commit":{"message":"commit %s"}}`, branch, sha, repo, sha[:7]))
	req, _ := http.NewRequest("POST", e.srv.URL+"/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", webhook.Sign(secret, body))
	var out map[string]any
	code := e.send(req, &out)
	return code, out
}

func (e *env) send(req *http.Request, out any) int {
	e.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatalf("%s %s: decode: %v", req.Method, req.URL.Path, err)
		}
	}
	return resp.StatusCode
}

// drain runs the worker until the queue is empty.
func (e *env) drain() {
	e.t.Helper()
	for {
		worked, err := e.wk.ProcessOne(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if !worked {
			return
		}
	}
}

func sha(n int) string { return fmt.Sprintf("%040x", n) }

// TestDeployFlow walks the Faz 1 happy path end to end:
// create app → push main → ready + production alias → push branch → preview
// alias → push main again → rollback to the first deployment.
func TestDeployFlow(t *testing.T) {
	e := setup(t, worker.DryRunPipeline{Registry: "reg"})

	var app map[string]any
	if code := e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, &app); code != 201 {
		t.Fatalf("create app: %d %v", code, app)
	}
	if app["production_url"] != "https://blog.paas.test" {
		t.Fatalf("production_url = %v", app["production_url"])
	}

	// 1st push to main.
	code, d1 := e.push("NisaGwn/Blog", "main", sha(1)) // repo match is case-insensitive
	if code != 201 || d1["status"] != "queued" {
		t.Fatalf("push 1: %d %v", code, d1)
	}
	if d1["url"] != "https://0000000-blog.paas.test" {
		t.Fatalf("deployment url = %v", d1["url"])
	}
	// Re-delivery of the same commit is idempotent.
	if code, again := e.push("nisagwn/blog", "main", sha(1)); code != 200 || again["id"] != d1["id"] {
		t.Fatalf("redelivery: %d %v", code, again)
	}
	e.drain()

	var got map[string]any
	e.do("GET", fmt.Sprintf("/api/deployments/%v", d1["id"]), nil, &got)
	if got["status"] != "ready" || got["image"] != "reg/blog:"+sha(1) {
		t.Fatalf("deployment 1 after worker: %v", got)
	}

	// Push a feature branch: preview alias only, production untouched.
	_, d2 := e.push("nisagwn/blog", "feature/login", sha(2))
	// 2nd push to main.
	_, d3 := e.push("nisagwn/blog", "main", sha(3))
	e.drain()

	aliases := e.aliases()
	if aliases["blog.paas.test"] != d3["id"] {
		t.Fatalf("production should follow latest main: %v", aliases)
	}
	if aliases["feature-login-blog.paas.test"] != d2["id"] {
		t.Fatalf("preview alias missing: %v", aliases)
	}
	if aliases["main-blog.paas.test"] != d3["id"] {
		t.Fatalf("main preview alias missing: %v", aliases)
	}

	// Instant rollback to the first deployment.
	var rb map[string]any
	if code := e.do("POST", "/api/apps/blog/rollback", map[string]any{"deployment_id": d1["id"]}, &rb); code != 200 {
		t.Fatalf("rollback: %d %v", code, rb)
	}
	if e.aliases()["blog.paas.test"] != d1["id"] {
		t.Fatal("production alias did not move to deployment 1")
	}

	// Logs were recorded.
	var logs []map[string]any
	e.do("GET", fmt.Sprintf("/api/deployments/%v/logs", d1["id"]), nil, &logs)
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1]["line"].(string), "ready: https://0000000-blog.paas.test") {
		t.Fatalf("unexpected logs: %v", logs)
	}
	// Incremental polling returns only newer lines.
	var tail []map[string]any
	e.do("GET", fmt.Sprintf("/api/deployments/%v/logs?after=%v", d1["id"], logs[len(logs)-2]["id"]), nil, &tail)
	if len(tail) != 1 {
		t.Fatalf("after= returned %d lines, want 1", len(tail))
	}
}

func (e *env) aliases() map[string]any {
	var app struct {
		Aliases []store.Alias `json:"aliases"`
	}
	e.do("GET", "/api/apps/blog", nil, &app)
	m := map[string]any{}
	for _, a := range app.Aliases {
		m[a.Hostname] = float64(a.DeploymentID) // JSON numbers decode as float64
	}
	return m
}

type failingPipeline struct{ worker.DryRunPipeline }

func (failingPipeline) Build(context.Context, store.Deployment, worker.Logger) (string, error) {
	return "", errors.New("npm install exited with code 1")
}

func TestFailedBuildCannotReceiveTraffic(t *testing.T) {
	e := setup(t, failingPipeline{})
	e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, nil)
	_, d := e.push("nisagwn/blog", "main", sha(1))
	e.drain()

	var got map[string]any
	e.do("GET", fmt.Sprintf("/api/deployments/%v", d["id"]), nil, &got)
	if got["status"] != "failed" || !strings.Contains(got["error"].(string), "npm install") {
		t.Fatalf("want failed with build error, got %v", got)
	}
	if len(e.aliases()) != 0 {
		t.Fatal("failed deployment must not get aliases")
	}
	var rb map[string]any
	if code := e.do("POST", "/api/apps/blog/rollback", map[string]any{"deployment_id": d["id"]}, &rb); code != 409 {
		t.Fatalf("rollback to failed deployment: %d %v, want 409", code, rb)
	}
}

func TestWebhookRejectsBadSignatureAndAuth(t *testing.T) {
	e := setup(t, worker.DryRunPipeline{})

	req, _ := http.NewRequest("POST", e.srv.URL+"/webhooks/github", strings.NewReader(`{}`))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=00")
	if code := e.send(req, nil); code != 401 {
		t.Fatalf("bad signature: %d, want 401", code)
	}

	req, _ = http.NewRequest("GET", e.srv.URL+"/api/apps", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	if code := e.send(req, nil); code != 401 {
		t.Fatalf("bad token: %d, want 401", code)
	}

	// Push for a repo with no app: accepted but ignored.
	if code, out := e.push("someone/else", "main", sha(9)); code != 202 || out["result"] != "ignored" {
		t.Fatalf("unknown repo: %d %v", code, out)
	}
}

func TestCreateAppValidation(t *testing.T) {
	e := setup(t, worker.DryRunPipeline{})
	for _, body := range []map[string]string{
		{"name": "Blog", "repo": "a/b"},
		{"name": "blog", "repo": "not-a-repo"},
	} {
		if code := e.do("POST", "/api/apps", body, nil); code != 400 {
			t.Errorf("%v: got %d, want 400", body, code)
		}
	}
	e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "a/b"}, nil)
	if code := e.do("POST", "/api/apps", map[string]string{"name": "blog2", "repo": "a/b"}, nil); code != 409 {
		t.Errorf("duplicate repo: got %d, want 409", code)
	}
}

func TestAppEnvAPI(t *testing.T) {
	e := setup(t, worker.DryRunPipeline{})
	e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, nil)

	var out map[string]any
	if code := e.do("PUT", "/api/apps/blog/env", map[string]any{"DB_URL": "postgres://secret", "DEBUG": "1"}, &out); code != 200 {
		t.Fatalf("put env: %d %v", code, out)
	}
	if code := e.do("PUT", "/api/apps/blog/env", map[string]any{"DEBUG": nil}, &out); code != 200 {
		t.Fatalf("delete env: %d %v", code, out)
	}

	// GET lists keys only; values never leave the API.
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/apps/blog/env", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != `{"keys":["DB_URL"]}` {
		t.Fatalf("get env: %d %s", resp.StatusCode, body)
	}

	for _, bad := range []map[string]any{
		{"1X": "v"}, {"A-B": "v"}, {"PORT": "9000"}, {"PAAS_APP": "x"},
		{"BIG": strings.Repeat("x", 33<<10)},
	} {
		if code := e.do("PUT", "/api/apps/blog/env", bad, nil); code != 400 {
			t.Errorf("%v: got %d, want 400", bad, code)
		}
	}
	if code := e.do("GET", "/api/apps/nope/env", nil, nil); code != 404 {
		t.Errorf("unknown app: got %d, want 404", code)
	}
}
