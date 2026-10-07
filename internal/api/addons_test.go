package api_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/addons"
	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/worker"
)

// syncBuffer collects the server log (the audit line of a reveal).
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// addon returns blog's Postgres add-on name, creating it if needed
// (access matrix).
func (f *teamFixture) addon(name string) store.Addon {
	f.t.Helper()
	ctx := context.Background()
	blog, _ := f.st.GetAppByName(ctx, "blog")
	a, err := f.st.GetAddon(ctx, blog.ID, name)
	if err == nil {
		return a
	}
	if a, err = f.st.CreateAddon(ctx, blog.ID, store.NewAddon{Kind: "postgres", Name: name, Plan: "hobby", PreviewMode: "copy"}); err != nil {
		f.t.Fatal(err)
	}
	return a
}

// readyAddon makes blog's "db" ready and idle: no rotation or backup running.
func (f *teamFixture) readyAddon() {
	f.t.Helper()
	ctx := context.Background()
	a := f.addon("db")
	f.st.SetAddonState(ctx, a.ID, store.AddonReady, "")
	f.st.CancelAddonRotation(ctx, a, "")
	backups, _ := f.st.ListAddonBackups(ctx, a.ID, 100)
	for _, b := range backups {
		f.st.SetBackupStatus(ctx, b.ID, store.BackupFailed, "test")
	}
}

// dropAddons removes blog's add-ons whose name starts with prefix.
func (f *teamFixture) dropAddons(prefix string) {
	ctx := context.Background()
	blog, _ := f.st.GetAppByName(ctx, "blog")
	list, _ := f.st.ListAddons(ctx, blog.ID)
	for _, a := range list {
		if strings.HasPrefix(a.Name, prefix) {
			f.st.DeleteAddon(ctx, a)
			f.st.RemoveAddon(ctx, a.ID)
		}
	}
}

// TestAddonsAPI walks the add-on API with the dry-run reconcile loop: create,
// settings, reveal (logged), a preview deployment with its own database,
// reset, rotation with redeploy, backups and restore, deletion.
func TestAddonsAPI(t *testing.T) {
	st := testdb.Open(t)
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	srv := httptest.NewServer((&api.Server{Store: st, Domain: domain, APIToken: token, WebhookSecret: secret, Log: log}).Handler())
	t.Cleanup(srv.Close)
	ctrl := addons.New(st, nil, log) // dry run: no cluster
	ctrl.Interval, ctrl.WaitPoll = 50*time.Millisecond, 20*time.Millisecond
	e := &env{t: t, srv: srv, wk: &worker.Worker{Store: st, Pipeline: worker.DryRunPipeline{}, Domain: domain, Log: log, Databases: ctrl}}
	ctx := context.Background()
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	reconcile := func() {
		t.Helper()
		app, _ := st.GetAppByName(ctx, "blog")
		list, _ := st.ListAddons(ctx, app.ID)
		for _, a := range list {
			if err := ctrl.Reconcile(ctx, a); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, nil)

	for _, bad := range []map[string]string{
		{"kind": "mysql"},
		{"kind": "postgres", "name": "Bad_Name"},
		{"kind": "postgres", "plan": "huge"},
		{"kind": "redis", "preview_mode": "copy"},
		{"kind": "postgres", "preview_mode": "clone"},
	} {
		if code := e.do("POST", "/api/apps/blog/addons", bad, nil); code != 400 {
			t.Errorf("create %v: %d", bad, code)
		}
	}
	var pg api.AddonView
	if code := e.do("POST", "/api/apps/blog/addons", map[string]string{"kind": "postgres"}, &pg); code != 201 ||
		pg.Name != "db" || pg.Plan != "hobby" || pg.PreviewMode != "copy" || pg.Status != "provisioning" ||
		pg.Host != "addon-db.app-blog.svc" || pg.User != "app" || !strings.Contains(strings.Join(pg.Variables, ","), "DATABASE_URL") {
		t.Fatalf("create: %d %+v", code, pg)
	}
	var cache api.AddonView
	if code := e.do("POST", "/api/apps/blog/addons", map[string]string{"kind": "redis"}, &cache); code != 201 ||
		cache.Name != "cache" || cache.PreviewMode != "shared" || strings.Join(cache.Variables, ",") != "REDIS_URL" {
		t.Fatalf("create redis: %d %+v", code, cache)
	}
	if code := e.do("POST", "/api/apps/blog/addons", map[string]string{"kind": "redis"}, nil); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	var analytics api.AddonView
	e.do("POST", "/api/apps/blog/addons", map[string]string{"kind": "postgres", "name": "analytics", "preview_mode": "shared"}, &analytics)
	if strings.Join(analytics.Variables, ",") != "ANALYTICS_DATABASE_URL,ANALYTICS_PGDATABASE,ANALYTICS_PGHOST,ANALYTICS_PGPASSWORD,ANALYTICS_PGPORT,ANALYTICS_PGUSER" {
		t.Fatalf("prefixed variables: %v", analytics.Variables)
	}

	// Settings: rules are validated and normalized; the volume cannot shrink.
	for _, bad := range []map[string]any{
		{"anonymize": []string{"users.email: shuffle"}},
		{"anonymize": []string{`users.email"; DROP TABLE users; --: null`}},
		{"anonymize_sql": `\! cat /etc/passwd`},
		{"backup_keep": 31},
		{"plan": "nope"},
	} {
		if code := e.do("PATCH", "/api/apps/blog/addons/db", bad, nil); code != 400 {
			t.Errorf("patch %v: %d", bad, code)
		}
	}
	if code := e.do("PATCH", "/api/apps/blog/addons/cache", map[string]any{"anonymize": []string{"users.email: email"}}, nil); code != 400 {
		t.Errorf("rules on redis: %d", code)
	}
	if code := e.do("PATCH", "/api/apps/blog/addons/db", map[string]any{
		"plan": "standard", "anonymize": []string{"public.users.email: email", "", "*.phone: null"},
		"anonymize_sql": "UPDATE orders SET note = NULL;", "backup_keep": 3,
	}, &pg); code != 200 || pg.Plan != "standard" || strings.Join(pg.Anonymize, "|") != "users.email: email|*.phone: null" || pg.BackupKeep != 3 {
		t.Fatalf("patch: %d %+v", code, pg)
	}
	if code := e.do("PATCH", "/api/apps/blog/addons/db", map[string]any{"plan": "hobby"}, nil); code != 400 {
		t.Errorf("shrinking plan: %d", code)
	}
	if code := e.do("POST", "/api/apps/blog/addons/db/rotate", nil, nil); code != 409 {
		t.Errorf("rotate before ready: %d", code)
	}

	// Reveal is logged with the caller.
	var creds api.AddonCredentials
	if code := e.do("POST", "/api/apps/blog/addons/db/credentials/reveal", nil, &creds); code != 200 ||
		creds.User != "app" || len(creds.Password) != 40 || creds.Variables["PGPASSWORD"] != creds.Password ||
		creds.URL != "postgres://app:"+creds.Password+"@addon-db.app-blog.svc:5432/app?sslmode=disable" {
		t.Fatalf("reveal: %d %+v", code, creds)
	}
	if !strings.Contains(logs.String(), `msg="addon credentials revealed" app=blog addon=db branch="" by=admin`) {
		t.Errorf("no audit line:\n%s", logs.String())
	}
	var list []api.AddonView
	if e.do("GET", "/api/apps/blog/addons", nil, &list); len(list) != 3 || strings.Contains(fmt.Sprint(list), creds.Password) {
		t.Fatalf("list leaks the password or is wrong: %+v", list)
	}

	// A preview deployment gets its own database before it is deployed;
	// from here on the loop runs (the worker waits for it).
	reconcile()
	go ctrl.Run(runCtx)
	e.push("nisagwn/blog", "feature/x", sha(1))
	e.drain()
	var branches []store.AddonBranch
	if e.do("GET", "/api/apps/blog/addons/db/branches", nil, &branches); len(branches) != 1 ||
		branches[0].Branch != "feature/x" || branches[0].Status != "ready" {
		t.Fatalf("branches: %+v", branches)
	}
	var bcreds api.AddonCredentials
	if code := e.do("POST", "/api/apps/blog/addons/db/credentials/reveal", map[string]string{"branch": "feature/x"}, &bcreds); code != 200 ||
		bcreds.Database != branches[0].Database || bcreds.User != branches[0].Database || bcreds.Password == creds.Password {
		t.Fatalf("branch reveal: %d %+v", code, bcreds)
	}
	if code := e.do("POST", "/api/apps/blog/addons/db/credentials/reveal", map[string]string{"branch": "nope"}, nil); code != 404 {
		t.Errorf("reveal unknown branch: %d", code)
	}
	if code := e.do("POST", "/api/apps/blog/addons/db/branches/feature%2Fx/reset", nil, nil); code != 202 {
		t.Fatalf("reset: %d", code)
	}
	if code := e.do("POST", "/api/apps/blog/addons/db/branches/feature%2Fx/reset", nil, nil); code != 409 {
		t.Fatalf("second reset: %d", code)
	}
	if code := e.do("POST", "/api/apps/blog/addons/analytics/branches/feature%2Fx/reset", nil, nil); code != 409 {
		t.Fatalf("reset shared: %d", code)
	}
	reconcile()

	// Rotation: the production deployment is redeployed with the new password.
	e.push("nisagwn/blog", "main", sha(2))
	e.drain()
	var rot map[string]any
	if code := e.do("POST", "/api/apps/blog/addons/db/rotate", map[string]bool{"redeploy": true}, &rot); code != 202 || rot["redeploy"] != true {
		t.Fatalf("rotate: %d %v", code, rot)
	}
	if code := e.do("POST", "/api/apps/blog/addons/db/rotate", nil, nil); code != 409 {
		t.Errorf("second rotate: %d", code)
	}
	reconcile()
	var deps []map[string]any
	e.do("GET", "/api/apps/blog/deployments", nil, &deps)
	if deps[0]["origin"] != "redeploy" || deps[0]["status"] != "queued" {
		t.Fatalf("no redeploy after rotation: %v", deps[0])
	}
	var creds2 api.AddonCredentials
	e.do("POST", "/api/apps/blog/addons/db/credentials/reveal", nil, &creds2)
	if creds2.Password == creds.Password {
		t.Fatal("password unchanged")
	}

	// Backups and restore (with the name typed to confirm).
	if code := e.do("POST", "/api/apps/blog/addons/cache/backups", nil, nil); code != 400 {
		t.Errorf("redis backup: %d", code)
	}
	var b store.AddonBackup
	if code := e.do("POST", "/api/apps/blog/addons/db/backups", nil, &b); code != 202 || b.Status != "pending" {
		t.Fatalf("backup: %d %+v", code, b)
	}
	reconcile()
	var backups []store.AddonBackup
	if e.do("GET", "/api/apps/blog/addons/db/backups", nil, &backups); len(backups) != 1 || backups[0].Status != "succeeded" {
		t.Fatalf("backups: %+v", backups)
	}
	path := fmt.Sprintf("/api/apps/blog/addons/db/backups/%d/restore", b.ID)
	if code := e.do("POST", path, map[string]string{"confirm": "wrong"}, nil); code != 400 {
		t.Errorf("restore without confirmation: %d", code)
	}
	if code := e.do("POST", path, map[string]string{"confirm": "db"}, &b); code != 202 || b.RestoreStatus != "pending" {
		t.Fatalf("restore: %d %+v", code, b)
	}
	if code := e.do("POST", "/api/apps/blog/addons/db/backups/99999/restore", map[string]string{"confirm": "db"}, nil); code != 404 {
		t.Errorf("restore unknown: %d", code)
	}

	// Deletion needs the name; the reconcile loop removes the row.
	if code := e.do("DELETE", "/api/apps/blog/addons/db", nil, nil); code != 400 {
		t.Errorf("delete without confirmation: %d", code)
	}
	if code := e.do("DELETE", "/api/apps/blog/addons/db?confirm=db", nil, &pg); code != 202 || pg.Status != "deleting" {
		t.Fatalf("delete: %d %+v", code, pg)
	}
	if code := e.do("PATCH", "/api/apps/blog/addons/db", map[string]any{"backup_keep": 1}, nil); code != 409 {
		t.Errorf("patch while deleting: %d", code)
	}
	reconcile()
	if code := e.do("GET", "/api/apps/blog/addons/db", nil, nil); code != 404 {
		t.Fatalf("after deletion: %d", code)
	}
	if code := e.do("GET", "/api/apps/blog/addons/zzz/branches", nil, nil); code != 404 {
		t.Errorf("unknown add-on: %d", code)
	}
}
