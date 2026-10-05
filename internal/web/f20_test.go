package web_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/web"
)

// Faz 20: app tabs, deploy controls, build settings, environments, deploy
// hooks and analytics in the UI.

type f20 struct {
	*ui
	srv           *web.Server
	app           store.App
	owner, viewer *ui
}

// setupF20 creates team "web" with owner alice and viewer carol, and app
// blog (production branch main).
func setupF20(t *testing.T, edit func(*web.Server)) *f20 {
	st := testdb.Open(t)
	s := &web.Server{
		Store: st, Sessions: auth.New(token), Domain: "paas.test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if edit != nil {
		edit(s)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	u := &ui{t: t, st: st, srv: srv, client: newClient()}
	ctx := context.Background()
	alice, _ := st.UpsertUser(ctx, 1, "alice", "", "")
	carol, _ := st.UpsertUser(ctx, 2, "carol", "", "")
	team, _ := st.CreateTeam(ctx, "web", "Web", alice.ID)
	st.SetMember(ctx, team.ID, carol.ID, store.RoleViewer)
	app, err := st.CreateAppInTeam(ctx, team.ID, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	return &f20{ui: u, srv: s, app: app, owner: u.as(alice.ID), viewer: u.as(carol.ID)}
}

func mustContain(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("%s lacks %q", what, w)
		}
	}
}

func mustNotContain(t *testing.T, what, body string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(body, w) {
			t.Errorf("%s contains %q", what, w)
		}
	}
}

// formChecks asserts that path rejects a POST without CSRF, a cross-origin
// POST and a viewer's POST.
func (f *f20) formChecks(path string, form url.Values) {
	f.t.Helper()
	if code, _, _ := f.owner.post(path, form, "self"); code != http.StatusForbidden {
		f.t.Errorf("%s without CSRF: %d, want 403", path, code)
	}
	withCSRF := func(c *ui) url.Values {
		v := url.Values{"csrf": {c.csrf("/apps/blog")}}
		for k, vs := range form {
			v[k] = vs
		}
		return v
	}
	if code, _, _ := f.owner.post(path, withCSRF(f.owner), "https://evil.example"); code != http.StatusForbidden {
		f.t.Errorf("%s cross-origin: %d, want 403", path, code)
	}
	if code, _, _ := f.viewer.post(path, withCSRF(f.viewer), "self"); code != http.StatusForbidden {
		f.t.Errorf("%s as viewer: %d, want 403", path, code)
	}
}

func TestAppTabs(t *testing.T) {
	f := setupF20(t, nil)
	for _, p := range []string{"/apps/blog", "/apps/blog/deployments", "/apps/blog/analytics", "/apps/blog/settings"} {
		for _, c := range []*ui{f.owner, f.viewer} {
			code, body, _ := c.get(p)
			if code != 200 {
				t.Fatalf("GET %s: %d", p, code)
			}
			mustContain(t, p, body, `href="/apps/blog/analytics"`, `aria-current="page"`, ">Ayarlar</a>")
		}
	}
	// The deployments tab is also the htmx fragment.
	req, _ := http.NewRequest("GET", f.srv0()+"/apps/blog/deployments", nil)
	req.Header.Set("HX-Request", "true")
	resp, err := f.owner.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(strings.TrimSpace(string(b)), `<div id="deployments"`) {
		t.Fatalf("htmx fragment: %s", b)
	}
}

func (f *f20) srv0() string { return f.ui.srv.URL }

func TestDeployControls(t *testing.T) {
	f := setupF20(t, nil)
	ctx := context.Background()
	st := f.st
	prod := []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}}
	d1, _, _ := st.EnqueueDeployment(ctx, f.app.ID, sha(1), "main", "first")
	st.SetImage(ctx, d1.ID, "registry.test/blog@sha256:aaa")
	st.SetFramework(ctx, d1.ID, "Next.js")
	st.MarkReady(ctx, d1, prod)
	d2, _, _ := st.EnqueueDeployment(ctx, f.app.ID, sha(2), "feature", "preview work")
	st.SetImage(ctx, d2.ID, "registry.test/blog@sha256:bbb")
	st.MarkReady(ctx, d2, []store.AliasSpec{{Hostname: "feature-blog.paas.test", Kind: store.AliasPreview, Branch: "feature"}})
	d3, _, _ := st.EnqueueDeployment(ctx, f.app.ID, sha(3), "main", "queued one")

	const tab = "/apps/blog/deployments"
	_, body, _ := f.owner.get(tab)
	mustContain(t, "owner deployments", body, "Production'a taşı", "Yeniden deploy et", "Önbelleği kullanma", "İptal et",
		"Next.js", ">Önizleme<", ">Production<", "https://0000000-blog.paas.test")
	_, body, _ = f.viewer.get(tab)
	mustNotContain(t, "viewer deployments", body, "Production'a taşı", "Yeniden deploy et", "İptal et", "Bu sürüme dön")

	id := func(d store.Deployment) url.Values { return url.Values{"deployment_id": {fmt.Sprint(d.ID)}} }
	f.formChecks("/apps/blog/promote", id(d2))
	f.formChecks("/apps/blog/redeploy", id(d1))
	f.formChecks("/apps/blog/cancel", id(d3))
	if deps, _ := st.ListDeployments(ctx, f.app.ID, 50); len(deps) != 3 {
		t.Fatalf("rejected posts created deployments: %d", len(deps))
	}

	csrf := f.owner.csrf(tab)
	post := func(path string, d store.Deployment, extra ...string) (int, string, http.Header) {
		v := id(d)
		v.Set("csrf", csrf)
		for i := 0; i+1 < len(extra); i += 2 {
			v.Set(extra[i], extra[i+1])
		}
		return f.owner.post(path, v, "self")
	}

	// Promote the preview: a production copy (generation 1) with the image.
	code, _, h := post("/apps/blog/promote", d2)
	if code != http.StatusSeeOther || h.Get("Location") != tab+"?ok=promoted" {
		t.Fatalf("promote: %d %s", code, h.Get("Location"))
	}
	deps, _ := st.ListDeployments(ctx, f.app.ID, 50)
	p := deps[0]
	if p.Origin != store.OriginPromote || p.Target != store.EnvProduction || p.Generation != 1 || p.Image != "registry.test/blog@sha256:bbb" {
		t.Fatalf("promoted copy: %+v", p)
	}
	_, body, _ = f.owner.get(tab + "?ok=promoted")
	mustContain(t, "after promote", body, "taşınıyor: aynı imajla", "Production&#39;a taşındı")
	// Promoting a queued deployment is refused in Turkish.
	if code, body, _ := post("/apps/blog/promote", p); code != http.StatusConflict || !strings.Contains(body, "Yalnızca hazır deploy&#39;lar") {
		t.Errorf("promote queued: %d", code)
	}

	// Redeploy without cache rebuilds; with cache it reuses the image.
	if code, _, h := post("/apps/blog/redeploy", d1, "no_cache", "1"); code != http.StatusSeeOther || h.Get("Location") != tab+"?ok=redeploy" {
		t.Fatalf("redeploy: %d %s", code, h.Get("Location"))
	}
	deps, _ = st.ListDeployments(ctx, f.app.ID, 50)
	if r := deps[0]; r.Origin != store.OriginRedeploy || r.Image != "" || r.Target != store.EnvProduction || r.Generation != 1 {
		t.Fatalf("redeploy without cache: %+v", r)
	}
	post("/apps/blog/redeploy", d1)
	deps, _ = st.ListDeployments(ctx, f.app.ID, 50)
	if r := deps[0]; r.Image != "registry.test/blog@sha256:aaa" || r.Generation != 2 {
		t.Fatalf("redeploy with cache: %+v", r)
	}
	// A ready copy links its own URL (generation suffix), not the commit's first one.
	st.MarkReady(ctx, deps[0], prod)
	_, body, _ = f.owner.get(tab)
	mustContain(t, "ready copy", body, `href="https://0000000-2-blog.paas.test"`)
	if code, body, _ := post("/apps/blog/redeploy", d3); code != http.StatusConflict || !strings.Contains(body, "Deploy hâlâ sürüyor (Sırada)") {
		t.Errorf("redeploy queued: %d", code)
	}

	// Cancel: queued at once, running on request.
	if code, _, h := post("/apps/blog/cancel", d3); code != http.StatusSeeOther || h.Get("Location") != tab+"?ok=canceled" {
		t.Fatalf("cancel: %d %s", code, h.Get("Location"))
	}
	if d, _ := st.GetDeployment(ctx, d3.ID); d.Status != store.StatusCanceled {
		t.Fatalf("status after cancel: %s", d.Status)
	}
	_, body, _ = f.owner.get(tab)
	mustContain(t, "after cancel", body, `class="badge s-canceled">İptal edildi<`)
	if code, body, _ := post("/apps/blog/cancel", d3); code != http.StatusConflict || !strings.Contains(body, "Deploy zaten bitti (İptal edildi)") {
		t.Errorf("cancel twice: %d", code)
	}
	st.SetStatus(ctx, p.ID, store.StatusBuilding)
	if code, _, h := post("/apps/blog/cancel", p); code != http.StatusSeeOther || h.Get("Location") != tab+"?ok=cancel" {
		t.Fatalf("cancel running: %d %s", code, h.Get("Location"))
	}
	_, body, _ = f.owner.get(tab)
	mustContain(t, "cancel requested", body, "İptal isteniyor")
	// Another app's deployment is not found.
	other, _ := st.CreateApp(ctx, "other", "nisagwn/other", "main")
	od, _, _ := st.EnqueueDeployment(ctx, other.ID, sha(9), "main", "x")
	if code, _, _ := post("/apps/blog/cancel", od); code != http.StatusNotFound {
		t.Errorf("cancel other app's deployment: %d", code)
	}
}

func TestSettingsForm(t *testing.T) {
	f := setupF20(t, nil)
	ctx := context.Background()
	d, _, _ := f.st.EnqueueDeployment(ctx, f.app.ID, sha(1), "main", "x")
	f.st.SetFramework(ctx, d.ID, "Next.js")

	const tab = "/apps/blog/settings"
	_, body, _ := f.owner.get(tab)
	mustContain(t, "settings", body, "Otomatik algıla (Next.js)", `<option value="vite">Vite</option>`, "Build ayarları",
		"Ortam değişkenleri", "Deploy hook'ları", "Alan adları", "Uyku modu")
	_, body, _ = f.viewer.get(tab)
	mustContain(t, "viewer settings", body, "<fieldset disabled>", "üye rolü gerekir")
	mustNotContain(t, "viewer settings", body, `action="/apps/blog/env"`, `action="/apps/blog/hooks"`)

	form := url.Values{"framework": {"vite"}, "root_directory": {"./apps/web/"}, "node_version": {"v20"}, "build_command": {"npm run build:prod"}}
	f.formChecks(tab, form)
	if b, _ := f.st.GetBuildSettings(ctx, f.app.ID); b.UpdatedAt != nil {
		t.Fatal("rejected posts saved settings")
	}

	csrf := f.owner.csrf(tab)
	bad := url.Values{"csrf": {csrf}, "root_directory": {"../x"}, "build_command": {"make"}}
	code, body, _ := f.owner.post(tab, bad, "self")
	if code != http.StatusBadRequest {
		t.Fatalf("invalid root: %d", code)
	}
	mustContain(t, "invalid root", body, "Kök dizin repo içinde kalmalı", `value="../x"`, `value="make"`)
	code, body, _ = f.owner.post(tab, url.Values{"csrf": {csrf}, "node_version": {"twenty"}}, "self")
	if code != http.StatusBadRequest || !strings.Contains(body, "Node sürümü") {
		t.Errorf("invalid node version: %d", code)
	}
	code, body, _ = f.owner.post(tab, url.Values{"csrf": {csrf}, "framework": {"rails"}}, "self")
	if code != http.StatusBadRequest || !strings.Contains(body, "Framework boş (otomatik algıla)") {
		t.Errorf("invalid framework: %d", code)
	}

	form.Set("csrf", csrf)
	code, _, h := f.owner.post(tab, form, "self")
	if code != http.StatusSeeOther || h.Get("Location") != tab+"?ok=settings#build" {
		t.Fatalf("save: %d %s", code, h.Get("Location"))
	}
	b, _ := f.st.GetBuildSettings(ctx, f.app.ID)
	if b.Framework != "vite" || b.RootDirectory != "apps/web" || b.NodeVersion != "20" || b.BuildCommand != "npm run build:prod" {
		t.Fatalf("saved: %+v", b)
	}
	_, body, _ = f.owner.get(tab + "?ok=settings")
	mustContain(t, "after save", body, "Build ayarları kaydedildi", `<option value="vite" selected>`, `value="apps/web"`)
}

func TestEnvironmentForms(t *testing.T) {
	f := setupF20(t, nil)
	ctx := context.Background()
	const tab, path = "/apps/blog/settings", "/apps/blog/env"
	f.formChecks(path, url.Values{"key": {"A"}, "value": {"1"}})

	csrf := f.owner.csrf(tab)
	set := func(key, value, target, branch string) (int, string) {
		code, body, _ := f.owner.post(path, url.Values{"csrf": {csrf}, "key": {key}, "value": {value},
			"target": {target}, "git_branch": {branch}}, "self")
		return code, body
	}
	for _, c := range [][4]string{
		{"SENTRY_DSN", "s1", "", ""}, {"DATABASE_URL", "prod-secret", "production", ""},
		{"DATABASE_URL", "preview-secret", "preview", ""}, {"FLAG", "on", "preview", "feature/x"},
	} {
		if code, body := set(c[0], c[1], c[2], c[3]); code != http.StatusSeeOther {
			t.Fatalf("set %v: %d\n%s", c, code, body)
		}
	}
	if code, body := set("X", "1", "production", "feature/x"); code != http.StatusSeeOther {
		// The branch field only applies to previews and is ignored here.
		t.Fatalf("production with a stale branch field: %d\n%s", code, body)
	}
	if code, body := set("X", "1", "staging", ""); code != http.StatusBadRequest || !strings.Contains(body, "Ortam Tümü, Production ya da Önizleme olmalı") {
		t.Errorf("bad target: %d", code)
	}
	if code, body := set("PORT", "1", "", ""); code != http.StatusBadRequest || !strings.Contains(body, "platform tarafından ayarlanır") {
		t.Errorf("reserved key: %d", code)
	}

	prod, _ := f.st.ResolveEnv(ctx, f.app.ID, store.EnvProduction, "main")
	feat, _ := f.st.ResolveEnv(ctx, f.app.ID, store.EnvPreview, "feature/x")
	if prod["DATABASE_URL"] != "prod-secret" || prod["SENTRY_DSN"] != "s1" || feat["DATABASE_URL"] != "preview-secret" ||
		feat["FLAG"] != "on" || prod["FLAG"] != "" {
		t.Fatalf("resolved: prod %v, feature %v", prod, feat)
	}
	_, body, _ := f.owner.get(tab)
	mustContain(t, "env listing", body, ">Tümü <", ">Production <", ">Önizleme <", "feature/x", "DATABASE_URL")
	mustNotContain(t, "env listing", body, "prod-secret", "preview-secret")

	// Deleting the preview row keeps the production one.
	code, _, _ := f.owner.post(path+"/delete", url.Values{"csrf": {csrf}, "key": {"DATABASE_URL"}, "target": {"preview"}}, "self")
	if code != http.StatusSeeOther {
		t.Fatalf("delete preview row: %d", code)
	}
	vars, _ := f.st.ListEnv(ctx, f.app.ID)
	var targets []string
	for _, v := range vars {
		if v.Key == "DATABASE_URL" {
			targets = append(targets, v.Target)
		}
	}
	if len(targets) != 1 || targets[0] != store.EnvProduction {
		t.Fatalf("DATABASE_URL rows after delete: %v", targets)
	}
	if code, _, _ := f.viewer.post(path+"/delete", url.Values{"csrf": {f.viewer.csrf(tab)}, "key": {"SENTRY_DSN"}}, "self"); code != http.StatusForbidden {
		t.Errorf("viewer delete: %d", code)
	}
}

func TestDeployHookForms(t *testing.T) {
	f := setupF20(t, nil)
	ctx := context.Background()
	const tab, path = "/apps/blog/settings", "/apps/blog/hooks"
	f.formChecks(path, url.Values{"name": {"cms"}})

	csrf := f.owner.csrf(tab)
	code, body, _ := f.owner.post(path, url.Values{"csrf": {csrf}, "name": {"cms"}}, "self")
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	mustContain(t, "created hook", body, "Bu URL yalnızca şimdi gösterilir", f.srv0()+"/api/hooks/deploy/paas_hook_",
		"GitHub App ayarlı olmadığı için")
	hooks, _ := f.st.ListDeployHooks(ctx, f.app.ID)
	if len(hooks) != 1 || hooks[0].Name != "cms" || hooks[0].Branch != "main" {
		t.Fatalf("hooks: %+v", hooks)
	}
	// Listed by prefix only, never the token again; viewers see no list.
	_, body, _ = f.owner.get(tab)
	mustContain(t, "hook listing", body, hooks[0].Prefix+"…", ">cms<")
	mustNotContain(t, "hook listing", body, "/api/hooks/deploy/")
	_, body, _ = f.viewer.get(tab)
	mustNotContain(t, "viewer hook listing", body, hooks[0].Prefix)

	if code, body, _ := f.owner.post(path, url.Values{"csrf": {csrf}, "branch": {"a b"}}, "self"); code != http.StatusBadRequest ||
		!strings.Contains(body, "geçerli bir branch adı") {
		t.Errorf("bad branch: %d", code)
	}

	del := url.Values{"id": {fmt.Sprint(hooks[0].ID)}}
	f.formChecks(path+"/delete", del)
	del.Set("csrf", csrf)
	if code, _, h := f.owner.post(path+"/delete", del, "self"); code != http.StatusSeeOther || h.Get("Location") != tab+"?ok=hook-deleted#hooks" {
		t.Fatalf("delete: %d %s", code, h.Get("Location"))
	}
	if hooks, _ := f.st.ListDeployHooks(ctx, f.app.ID); len(hooks) != 0 {
		t.Fatal("hook not deleted")
	}
	if code, _, _ := f.owner.post(path+"/delete", del, "self"); code != http.StatusNotFound {
		t.Errorf("delete missing: %d", code)
	}
}

type fakeUsage struct {
	pods []deploy.PodUsage
	err  error
}

func (u fakeUsage) AppUsage(context.Context, string) ([]deploy.PodUsage, error) { return u.pods, u.err }

func TestAnalyticsPage(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 30, 0, 0, time.UTC)
	usage := &fakeUsage{}
	f := setupF20(t, func(s *web.Server) {
		s.Now = func() time.Time { return now }
		s.Usage = usage
	})
	ctx := context.Background()
	prod := []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}}
	d1, _, _ := f.st.EnqueueDeployment(ctx, f.app.ID, sha(1), "main", "x")
	f.st.MarkReady(ctx, d1, prod)
	d2, _, _ := f.st.EnqueueDeployment(ctx, f.app.ID, sha(2), "feature", "y")
	f.st.MarkReady(ctx, d2, []store.AliasSpec{{Hostname: "feature-blog.paas.test", Kind: store.AliasPreview, Branch: "feature"}})
	var buckets []store.RequestBucket
	for i := 1; i <= 5; i++ {
		buckets = append(buckets, store.RequestBucket{
			DeploymentID: d1.ID, Minute: now.Add(-time.Duration(i) * time.Minute), Requests: 1000,
			Classes: [4]int64{800, 0, 0, 200}, DurationSum: 50, DurationCount: 1000,
			Buckets: map[float64]float64{0.05: 900, 0.5: 990, 1e308: 1000},
		})
	}
	if err := f.st.AddRequestMetrics(ctx, buckets); err != nil {
		t.Fatal(err)
	}
	usage.err = deploy.ErrNoMetricsAPI

	code, body, _ := f.viewer.get("/apps/blog/analytics?range=1h")
	if code != 200 {
		t.Fatalf("analytics: %d", code)
	}
	mustContain(t, "analytics", body,
		`<svg class="chart"`, `class="bar-err"`, "12:25 · 1.000 istek · 200 hata (5xx)", // a step's tooltip
		">5.000<", "%20,0", "1.000 hata", // totals
		`href="/apps/blog/analytics?range=1h" class="on"`,
		`class="badge h-failing">Çöküyor<`, `class="badge h-no_traffic">Trafik yok<`,
		"Kullanım verisi yok: kümede metrics-server çalışmıyor.")

	// No usage source (dry run) and an empty range.
	usage.err = nil
	usage.pods = []deploy.PodUsage{
		{Pod: "p1", DeploymentID: d1.ID, CPUMillicores: 120, MemoryBytes: 64 << 20, CPULimitMillicores: 500, MemoryLimitBytes: 256 << 20},
		{Pod: "p2", DeploymentID: d1.ID, CPUMillicores: 130, MemoryBytes: 64 << 20},
	}
	_, body, _ = f.owner.get("/apps/blog/analytics?range=7d")
	mustContain(t, "usage", body, "250m", "128 MiB", "/ 500m", "/ 256 MiB", `range=7d" class="on"`)
	now = now.Add(48 * time.Hour)
	_, body, _ = f.owner.get("/apps/blog/analytics?range=1h")
	mustContain(t, "empty range", body, "Bu aralıkta istek yok", `class="badge h-no_traffic"`)

	f.srv.Usage = nil // dry run
	_, body, _ = f.owner.get("/apps/blog/analytics?range=bogus")
	mustContain(t, "dry run", body, "Kullanım verisi yok: canlı CPU ve bellek yalnızca Kubernetes", `range=24h" class="on"`)
}
