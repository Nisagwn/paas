package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/webhook"
	"github.com/nisagwn/paas/internal/worker"
)

// recordingPipeline is a dry run that counts builds and records the
// variables each deployment would run with, as the Kubernetes deployer
// loads them (store.DeploymentEnv).
type recordingPipeline struct {
	st *store.Store

	mu     sync.Mutex
	builds int
	envs   map[int64]map[string]string
}

func (p *recordingPipeline) Build(_ context.Context, d store.Deployment, _ store.BuildSettings, _ worker.Logger) (worker.BuildResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.builds++
	return worker.BuildResult{Image: fmt.Sprintf("reg/%s:%s@sha256:%d", d.AppName, d.CommitSHA, p.builds)}, nil
}

func (p *recordingPipeline) Deploy(ctx context.Context, d store.Deployment, _ string, _ worker.Logger) error {
	env, err := p.st.DeploymentEnv(ctx, d)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.envs[d.ID] = env
	return nil
}

func setupControls(t *testing.T, repos api.RepoInspector) (*env, *recordingPipeline, *store.Store) {
	st := testdb.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &api.Server{Store: st, Domain: domain, APIToken: token, WebhookSecret: secret, Log: log}
	if repos != nil {
		srv.GitHubApp = repos
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	p := &recordingPipeline{st: st, envs: map[int64]map[string]string{}}
	e := &env{t: t, srv: ts, wk: &worker.Worker{Store: st, Pipeline: p, Domain: domain, Log: log}}
	if code := e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, nil); code != 201 {
		t.Fatalf("create app: %d", code)
	}
	return e, p, st
}

func id(v any) int64 { return int64(v.(float64)) }

// Promote a ready preview to production without a rebuild: a production
// copy of the same image runs with the production variables; the branch
// preview keeps the preview build.
func TestPromoteAndRedeploy(t *testing.T) {
	e, p, st := setupControls(t, nil)
	if code := e.do("PUT", "/api/apps/blog/env", map[string]any{"SHARED": "s"}, nil); code != 200 {
		t.Fatalf("env both: %d", code)
	}
	for _, body := range []map[string]any{
		{"target": "production", "vars": map[string]any{"DB": "prod"}},
		{"target": "preview", "vars": map[string]any{"DB": "preview"}},
		{"target": "preview", "git_branch": "feature/x", "vars": map[string]any{"FLAG": "on"}},
	} {
		var out map[string]any
		if code := e.do("PUT", "/api/apps/blog/env", body, &out); code != 200 {
			t.Fatalf("env %v: %d %v", body, code, out)
		}
	}
	for _, bad := range []map[string]any{
		{"target": "staging", "vars": map[string]any{"A": "1"}},
		{"target": "production", "git_branch": "main", "vars": map[string]any{"A": "1"}},
		{"target": "preview", "vars": map[string]any{"PORT": "1"}},
		{"vars": map[string]any{"A": "1"}, "extra": 1},
	} {
		if code := e.do("PUT", "/api/apps/blog/env", bad, nil); code != 400 {
			t.Errorf("%v: %d, want 400", bad, code)
		}
	}
	var envOut struct {
		Keys []string       `json:"keys"`
		Vars []store.EnvVar `json:"vars"`
	}
	e.do("GET", "/api/apps/blog/env", nil, &envOut)
	if strings.Join(envOut.Keys, ",") != "DB,FLAG,SHARED" || len(envOut.Vars) != 4 {
		t.Fatalf("env view: %+v", envOut)
	}

	_, prod := e.push("nisagwn/blog", "main", sha(1))
	_, feat := e.push("nisagwn/blog", "feature/x", sha(2))
	e.drain()
	if p.builds != 2 {
		t.Fatalf("builds = %d", p.builds)
	}
	if got := p.envs[id(feat["id"])]; got["DB"] != "preview" || got["FLAG"] != "on" || got["SHARED"] != "s" {
		t.Fatalf("preview env: %v", got)
	}
	if got := p.envs[id(prod["id"])]; got["DB"] != "prod" || got["FLAG"] != "" || got["SHARED"] != "s" {
		t.Fatalf("production env: %v", got)
	}

	// Promote the preview.
	var pr struct {
		Mode       string         `json:"mode"`
		Deployment map[string]any `json:"deployment"`
	}
	if code := e.do("POST", "/api/apps/blog/promote", map[string]any{"deployment_id": feat["id"]}, &pr); code != 202 || pr.Mode != "deployment" {
		t.Fatalf("promote: %d %+v", code, pr)
	}
	pd := pr.Deployment
	if pd["target"] != "production" || pd["origin"] != "promote" || pd["generation"] != float64(1) ||
		pd["url"] != "https://0000000-1-blog.paas.test" || id(pd["source_deployment_id"]) != id(feat["id"]) {
		t.Fatalf("promoted deployment: %v", pd)
	}
	e.drain()
	if p.builds != 2 {
		t.Fatalf("promotion rebuilt: builds = %d", p.builds)
	}
	if got := p.envs[id(pd["id"])]; got["DB"] != "prod" || got["FLAG"] != "" {
		t.Fatalf("promoted env: %v", got)
	}
	var promoted map[string]any
	e.do("GET", fmt.Sprintf("/api/deployments/%v", pd["id"]), nil, &promoted)
	var src map[string]any
	e.do("GET", fmt.Sprintf("/api/deployments/%v", feat["id"]), nil, &src)
	if promoted["status"] != "ready" || promoted["image"] != src["image"] {
		t.Fatalf("promoted: %v (source image %v)", promoted, src["image"])
	}
	aliases := e.aliases()
	if aliases["blog.paas.test"] != pd["id"] || aliases["feature-x-blog.paas.test"] != feat["id"] {
		t.Fatalf("aliases after promotion: %v", aliases)
	}

	// Promoting a deployment that already runs with production variables
	// only moves the alias (like a rollback).
	var back struct {
		Mode  string      `json:"mode"`
		Alias store.Alias `json:"alias"`
	}
	if code := e.do("POST", "/api/apps/blog/promote", map[string]any{"deployment_id": prod["id"]}, &back); code != 200 ||
		back.Mode != "alias" || back.Alias.DeploymentID != id(prod["id"]) {
		t.Fatalf("promote production deployment: %d %+v", code, back)
	}

	// Redeploy: same commit, image reused by default.
	var rd map[string]any
	if code := e.do("POST", fmt.Sprintf("/api/apps/blog/deployments/%v/redeploy", feat["id"]), nil, &rd); code != 202 {
		t.Fatalf("redeploy: %d %v", code, rd)
	}
	if rd["origin"] != "redeploy" || rd["target"] != "preview" || rd["image"] != src["image"] || rd["generation"] != float64(2) {
		t.Fatalf("redeploy: %v", rd)
	}
	e.drain()
	if p.builds != 2 || e.aliases()["feature-x-blog.paas.test"] != rd["id"] {
		t.Fatalf("redeploy rebuilt or preview did not move: builds=%d %v", p.builds, e.aliases())
	}
	// use_cache false forces a rebuild.
	var rb map[string]any
	if code := e.do("POST", fmt.Sprintf("/api/apps/blog/deployments/%v/redeploy", prod["id"]),
		map[string]any{"use_cache": false}, &rb); code != 202 || rb["image"] != nil {
		t.Fatalf("redeploy without cache: %d %v", code, rb)
	}
	e.drain()
	if p.builds != 3 || e.aliases()["blog.paas.test"] != rb["id"] {
		t.Fatalf("rebuild: builds=%d aliases=%v", p.builds, e.aliases())
	}

	// Errors: unknown, in flight, not ready, other app, bad body.
	_, queued := e.push("nisagwn/blog", "feature/y", sha(3))
	if code := e.do("POST", fmt.Sprintf("/api/apps/blog/deployments/%v/redeploy", queued["id"]), nil, nil); code != 409 {
		t.Fatalf("redeploy queued: %d", code)
	}
	if code := e.do("POST", "/api/apps/blog/promote", map[string]any{"deployment_id": queued["id"]}, nil); code != 409 {
		t.Fatalf("promote queued: %d", code)
	}
	if code := e.do("POST", "/api/apps/blog/promote", map[string]any{"deployment_id": 999999}, nil); code != 404 {
		t.Fatalf("promote unknown: %d", code)
	}
	if code := e.do("POST", "/api/apps/blog/deployments/1/redeploy", map[string]any{"nope": 1}, nil); code != 400 {
		t.Fatalf("redeploy bad body: %d", code)
	}
	st.CreateApp(context.Background(), "shop", "nisagwn/shop", "main")
	if code := e.do("POST", fmt.Sprintf("/api/apps/shop/deployments/%v/redeploy", feat["id"]), nil, nil); code != 404 {
		t.Fatalf("redeploy through another app: %d", code)
	}
}

func TestCancelAPI(t *testing.T) {
	e, _, st := setupControls(t, nil)
	_, q := e.push("nisagwn/blog", "main", sha(1))
	var out map[string]any
	if code := e.do("POST", fmt.Sprintf("/api/deployments/%v/cancel", q["id"]), nil, &out); code != 200 || out["status"] != "canceled" {
		t.Fatalf("cancel queued: %d %v", code, out)
	}
	if code := e.do("POST", fmt.Sprintf("/api/deployments/%v/cancel", q["id"]), nil, nil); code != 409 {
		t.Fatalf("cancel twice: %d", code)
	}
	e.drain() // nothing to run
	if a := e.aliases(); len(a) != 0 {
		t.Fatalf("aliases: %v", a)
	}

	_, b := e.push("nisagwn/blog", "main", sha(2))
	ctx := context.Background()
	claimed, _ := st.ClaimNext(ctx)
	if claimed.ID != id(b["id"]) {
		t.Fatal("claim")
	}
	if code := e.do("POST", fmt.Sprintf("/api/deployments/%v/cancel", b["id"]), nil, &out); code != 202 ||
		out["status"] != "building" || out["cancel_requested_at"] == nil {
		t.Fatalf("cancel building: %d %v", code, out)
	}
	if code := e.do("POST", "/api/deployments/999999/cancel", nil, nil); code != 404 {
		t.Fatalf("cancel unknown: %d", code)
	}
	// A redeploy of a canceled deployment builds it.
	var rd map[string]any
	if code := e.do("POST", fmt.Sprintf("/api/apps/blog/deployments/%v/redeploy", q["id"]), nil, &rd); code != 202 {
		t.Fatalf("redeploy canceled: %d %v", code, rd)
	}
}

// Deploy hooks: the token is shown once, stored hashed, and is the only
// credential of the trigger URL.
func TestDeployHooks(t *testing.T) {
	repos := &fakeRepos{heads: map[string]string{"nisagwn/blog@main": sha(1), "nisagwn/blog@docs": sha(5)}}
	e, p, _ := setupControls(t, repos)

	var h struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Branch string `json:"branch"`
		Token  string `json:"token"`
		Prefix string `json:"prefix"`
		Path   string `json:"path"`
	}
	if code := e.do("POST", "/api/apps/blog/hooks", map[string]string{"name": "cms"}, &h); code != 201 ||
		h.Branch != "main" || !strings.HasPrefix(h.Token, api.HookTokenPrefix) || h.Path != "/api/hooks/deploy/"+h.Token ||
		!strings.HasPrefix(h.Token, h.Prefix) {
		t.Fatalf("create hook: %d %+v", code, h)
	}
	// Stored hashed: neither listing nor the table holds the plain token.
	var list []map[string]any
	e.do("GET", "/api/apps/blog/hooks", nil, &list)
	if len(list) != 1 || list[0]["token"] != nil || list[0]["token_hash"] != nil {
		t.Fatalf("listing: %v", list)
	}
	db, err := sql.Open("postgres", os.Getenv("PAAS_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stored string
	db.QueryRow(`SELECT token_hash FROM deploy_hooks WHERE id = $1`, h.ID).Scan(&stored)
	if stored != auth.HashToken(h.Token) || strings.Contains(stored, h.Token) {
		t.Fatalf("stored %q", stored)
	}

	trigger := func(tok string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", e.srv.URL+api.HookPath(tok), nil) // no Authorization header
		var out map[string]any
		code := e.send(req, &out)
		return code, out
	}
	code, d := trigger(h.Token)
	if code != 201 || d["commit_sha"] != sha(1) || d["branch"] != "main" || d["origin"] != "hook" || d["target"] != "production" {
		t.Fatalf("trigger: %d %v", code, d)
	}
	// While it is queued, another trigger returns it.
	if code, again := trigger(h.Token); code != 200 || again["id"] != d["id"] {
		t.Fatalf("trigger while queued: %d %v", code, again)
	}
	e.drain()
	// The head was deployed already: the hook rebuilds it.
	code, d2 := trigger(h.Token)
	if code != 201 || d2["id"] == d["id"] || d2["generation"] != float64(1) || d2["image"] != nil {
		t.Fatalf("trigger after deploy: %d %v", code, d2)
	}
	e.drain()
	if p.builds != 2 || e.aliases()["blog.paas.test"] != d2["id"] {
		t.Fatalf("hook rebuild: builds=%d %v", p.builds, e.aliases())
	}
	e.do("GET", "/api/apps/blog/hooks", nil, &list)
	if list[0]["last_triggered_at"] == nil {
		t.Fatalf("last_triggered_at not set: %v", list)
	}

	// A preview hook.
	var docs struct {
		Token string `json:"token"`
	}
	e.do("POST", "/api/apps/blog/hooks", map[string]string{"branch": "docs"}, &docs)
	if code, d := trigger(docs.Token); code != 201 || d["target"] != "preview" || d["branch"] != "docs" {
		t.Fatalf("preview hook: %d %v", code, d)
	}

	for _, bad := range []string{"paas_hook_wrong", "nothook", h.Token + "x"} {
		if code, _ := trigger(bad); code != 404 {
			t.Fatalf("token %q: %d", bad, code)
		}
	}
	// Branch head unreadable.
	var gone struct {
		Token string `json:"token"`
	}
	e.do("POST", "/api/apps/blog/hooks", map[string]string{"branch": "deleted"}, &gone)
	if code, _ := trigger(gone.Token); code != 502 {
		t.Fatalf("missing branch: %d", code)
	}
	if code := e.do("POST", "/api/apps/blog/hooks", map[string]string{"branch": "bad branch"}, nil); code != 400 {
		t.Fatalf("bad branch: %d", code)
	}

	if code := e.do("DELETE", fmt.Sprintf("/api/apps/blog/hooks/%d", h.ID), nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code := e.do("DELETE", fmt.Sprintf("/api/apps/blog/hooks/%d", h.ID), nil, nil); code != 404 {
		t.Fatalf("delete twice: %d", code)
	}
	if code, _ := trigger(h.Token); code != 404 {
		t.Fatalf("deleted hook: %d", code)
	}
}

func TestDeployHookWithoutGitHubApp(t *testing.T) {
	e, _, _ := setupControls(t, nil)
	var h struct {
		Token string `json:"token"`
	}
	e.do("POST", "/api/apps/blog/hooks", map[string]string{}, &h)
	req, _ := http.NewRequest("POST", e.srv.URL+api.HookPath(h.Token), nil)
	if code := e.send(req, nil); code != 501 {
		t.Fatalf("without GitHub App: %d", code)
	}
}

// Ignored build step: "[skip deploy]" / "[skip ci]" anywhere in the head
// commit message keeps a push from deploying.
func TestSkipDeployCommits(t *testing.T) {
	e, _, st := setupControls(t, nil)
	pushMsg := func(sha, msg string) (int, map[string]any) {
		body := []byte(fmt.Sprintf(`{"ref":"refs/heads/main","after":"%s","repository":{"full_name":"nisagwn/blog"},
			"head_commit":{"message":%q}}`, sha, msg))
		req, _ := http.NewRequest("POST", e.srv.URL+"/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", webhook.Sign(secret, body))
		var out map[string]any
		return e.send(req, &out), out
	}
	for i, msg := range []string{"docs: typo [skip deploy]", "chore: bump\n\n[SKIP CI]", "[skip ci] readme"} {
		code, out := pushMsg(sha(10+i), msg)
		if code != 202 || out["result"] != "ignored" || !strings.Contains(out["reason"].(string), "[skip") {
			t.Fatalf("%q: %d %v", msg, code, out)
		}
	}
	app, _ := st.GetAppByName(context.Background(), "blog")
	if deps, _ := st.ListDeployments(context.Background(), app.ID, 10); len(deps) != 0 {
		t.Fatalf("skipped commits were queued: %+v", deps)
	}
	if code, out := pushMsg(sha(20), "feat: skip deploy button"); code != 201 {
		t.Fatalf("normal commit: %d %v", code, out)
	}
}
