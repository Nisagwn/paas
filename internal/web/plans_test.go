package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/ratelimit"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/web"
)

// planUI: setupF20 with a hobby plan of 1 project, 1 domain, 1 database,
// 2 replicas and the given deployments per day; alice is a platform admin.
func planUI(t *testing.T, edit func(*web.Server)) *f20 {
	t.Helper()
	cat := plans.DefaultCatalog()
	h := cat[store.PlanHobby]
	h.Projects, h.Domains, h.Addons, h.Replicas = 1, 1, 1, 2
	cat[store.PlanHobby] = h
	return setupF20(t, func(s *web.Server) {
		s.Plans = &plans.Guard{Store: s.Store, Catalog: cat}
		s.Auth = &auth.Authenticator{Store: s.Store, Sessions: s.Sessions, Log: s.Log, Admins: []string{"alice"}}
		s.Legal = web.Legal{ContactEmail: "kvkk@example.com", Operator: "Örnek Bulut"}
		if edit != nil {
			edit(s)
		}
	})
}

func (f *f20) teamID(t *testing.T) int64 {
	t.Helper()
	team, err := f.st.GetTeamBySlug(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	return team.ID
}

// form posts with the CSRF token of page as c.
func form(c *ui, page, path string, v url.Values) (int, string, http.Header) {
	c.t.Helper()
	if v == nil {
		v = url.Values{}
	}
	v.Set("csrf", c.csrf(page))
	return c.post(path, v, "self")
}

// Limit refusals of the web forms are shown in Turkish.
func TestPlanLimitsWeb(t *testing.T) {
	f := planUI(t, nil)
	ctx := context.Background()
	const tr = "Plan limitine ulaşıldı"

	code, body, _ := form(f.owner, "/", "/apps", url.Values{"name": {"shop"}, "repo": {"nisagwn/shop"}, "team": {"web"}})
	if code != http.StatusForbidden {
		t.Fatalf("create over the project limit: %d", code)
	}
	mustContain(t, "project limit", body, tr, "Hobby planında ekip başına en fazla 1 proje")

	if code, _, _ := form(f.owner, "/apps/blog/settings", "/apps/blog/domains", url.Values{"hostname": {"www.one.example"}}); code != http.StatusSeeOther {
		t.Fatalf("first domain: %d", code)
	}
	code, body, _ = form(f.owner, "/apps/blog/settings", "/apps/blog/domains", url.Values{"hostname": {"www.two.example"}})
	if code != http.StatusForbidden {
		t.Fatalf("second domain: %d", code)
	}
	mustContain(t, "domain limit", body, "en fazla 1 alan adı")

	f.st.CreateAddon(ctx, f.app.ID, store.NewAddon{Kind: "redis", Name: "cache", Plan: "hobby", PreviewMode: "shared"})
	code, body, _ = form(f.owner, "/apps/blog/databases", "/apps/blog/databases",
		url.Values{"kind": {"redis"}, "name": {"cache2"}, "plan": {"hobby"}})
	if code != http.StatusForbidden {
		t.Fatalf("second database: %d", code)
	}
	mustContain(t, "database limit", body, "en fazla 1 veritabanı")

	code, body, _ = form(f.owner, "/apps/blog/settings", "/apps/blog/processes/scale", url.Values{"process": {"queue"}, "replicas": {"3"}})
	if code != http.StatusForbidden {
		t.Fatalf("replicas: %d", code)
	}
	mustContain(t, "replica limit", body, "süreç başına en fazla 2 kopya")

	// Deployments per day: redeploy and promote.
	d1, _, _ := f.st.EnqueueDeployment(ctx, f.app.ID, sha(1), "main", "one")
	f.st.MarkReady(ctx, d1, []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}})
	d2, _, _ := f.st.EnqueueDeployment(ctx, f.app.ID, sha(2), "feature", "two")
	f.st.MarkReady(ctx, d2, []store.AliasSpec{{Hostname: "feature-blog.paas.test", Kind: store.AliasPreview, Branch: "feature"}})
	f.st.SetTeamPlan(ctx, f.teamID(t), store.PlanHobby, json.RawMessage(`{"deployments_per_day": 2}`))
	for _, path := range []string{"/apps/blog/redeploy", "/apps/blog/promote"} {
		id := d1.ID
		if strings.HasSuffix(path, "promote") {
			id = d2.ID
		}
		code, body, _ := form(f.owner, "/apps/blog/deployments", path, url.Values{"deployment_id": {fmt.Sprint(id)}})
		if code != http.StatusTooManyRequests {
			t.Fatalf("%s over the daily limit: %d", path, code)
		}
		mustContain(t, path, body, "günde en fazla 2 deploy", "00:00 UTC")
	}
	if deps, _ := f.st.ListDeployments(ctx, f.app.ID, 10); len(deps) != 2 {
		t.Fatalf("refused forms queued deployments: %d", len(deps))
	}
}

// The usage page shows the plan with bars; the team page links to it.
func TestUsagePage(t *testing.T) {
	f := planUI(t, nil)
	ctx := context.Background()
	d, _, _ := f.st.EnqueueDeployment(ctx, f.app.ID, sha(1), "main", "one")
	f.st.AddBuildSeconds(ctx, d.ID, 125)
	code, body, _ := f.viewer.get("/usage")
	if code != 200 {
		t.Fatalf("usage: %d", code)
	}
	mustContain(t, "usage page", body, "<h1>Kullanım</h1>", "Hobby", "Projeler", "1 / 1", "Build dakikası (bu ay)", "3 dk / 600 dk",
		"Deploy (bugün)", "1 / 50", "Veritabanları", "Alan adları", `class="meter full"`, `role="progressbar"`,
		"Build zaman aşımı", "15 dk", "2Gi")
	mustNotContain(t, "usage page", body, "Bu kurulumda plan limitleri uygulanmıyor")
	if _, body, _ := f.owner.get("/teams/web"); !strings.Contains(body, `href="/usage?team=web"`) {
		t.Fatal("team page lacks the usage link")
	}
	if code, _, _ := f.owner.get("/usage?team=ops"); code != 404 {
		t.Fatalf("other team's usage: %d", code)
	}
	// Suspended teams are flagged.
	f.st.SuspendTeam(ctx, f.teamID(t), "spam")
	_, body, _ = f.owner.get("/usage?team=web")
	mustContain(t, "suspended usage", body, "Bu ekip askıya alındı: spam")

	// Without plans the page still renders, with the default catalog.
	g := setupF20(t, nil)
	_, body, _ = g.owner.get("/usage")
	mustContain(t, "usage without plans", body, "Bu kurulumda plan limitleri uygulanmıyor", "Projeler")
}

// Only platform admins see and use the admin page.
func TestAdminPage(t *testing.T) {
	f := planUI(t, nil)
	ctx := context.Background()
	if code, body, _ := f.viewer.get("/admin"); code != http.StatusForbidden || !strings.Contains(body, "platform yöneticileri") {
		t.Fatalf("viewer: %d", code)
	}
	if _, body, _ := f.viewer.get("/usage"); strings.Contains(body, `href="/admin"`) {
		t.Fatal("non-admin sees the admin link")
	}
	code, body, _ := f.owner.get("/admin")
	if code != 200 {
		t.Fatalf("admin: %d", code)
	}
	mustContain(t, "admin page", body, "<h1>Yönetim</h1>", `id="team-web"`, "sahip: @alice", "Askıya al", `href="/admin"`)

	// Non-admins cannot post either.
	if code, _, _ := form(f.viewer, "/usage", "/admin/teams/web/plan", url.Values{"plan": {"pro"}}); code != http.StatusForbidden {
		t.Fatalf("viewer changes plan: %d", code)
	}

	code, _, h := form(f.owner, "/admin", "/admin/teams/web/plan", url.Values{"plan": {"pro"}, "limits": {"projects=50\nbuild_minutes=unlimited"}})
	if code != http.StatusSeeOther || h.Get("Location") != "/admin?ok=plan#team-web" {
		t.Fatalf("set plan: %d %s", code, h.Get("Location"))
	}
	tp, _ := f.st.GetTeamPlan(ctx, f.teamID(t))
	if tp.Plan != store.PlanPro || !strings.Contains(string(tp.Overrides), `"projects": 50`) || !strings.Contains(string(tp.Overrides), `"build_minutes": -1`) {
		t.Fatalf("plan: %+v %s", tp, tp.Overrides)
	}
	_, body, _ = f.owner.get("/admin?ok=plan")
	mustContain(t, "after plan", body, "Plan kaydedildi.", "build_minutes=unlimited\nprojects=50")

	code, body, _ = form(f.owner, "/admin", "/admin/teams/web/plan", url.Values{"plan": {"gold"}})
	if code != http.StatusBadRequest {
		t.Fatalf("bad plan: %d", code)
	}
	mustContain(t, "bad plan", body, "Plan Hobby, Pro ya da Sınırsız olmalı.")
	code, body, _ = form(f.owner, "/admin", "/admin/teams/web/plan", url.Values{"plan": {"pro"}, "limits": {"projects"}})
	if code != http.StatusBadRequest {
		t.Fatalf("bad limits: %d", code)
	}
	mustContain(t, "bad limits", body, "anahtar=değer")

	code, _, h = form(f.owner, "/admin", "/admin/teams/web/suspend", url.Values{"reason": {"kötüye kullanım"}})
	if code != http.StatusSeeOther || h.Get("Location") != "/admin?ok=suspended#team-web" {
		t.Fatalf("suspend: %d %s", code, h.Get("Location"))
	}
	_, body, _ = f.owner.get("/admin")
	mustContain(t, "suspended team", body, "Askıda", "kötüye kullanım", "Askıyı kaldır")
	if code, _, _ := form(f.owner, "/admin", "/admin/teams/web/unsuspend", nil); code != http.StatusSeeOther {
		t.Fatalf("unsuspend: %d", code)
	}
	if tp, _ := f.st.GetTeamPlan(ctx, f.teamID(t)); tp.Suspended() {
		t.Fatal("still suspended")
	}

	// Users: carol is suspended and listed; alice (an admin) cannot be.
	if code, _, _ := form(f.owner, "/admin", "/admin/users/suspend", url.Values{"login": {"@carol"}, "reason": {"bots"}}); code != http.StatusSeeOther {
		t.Fatalf("suspend user: %d", code)
	}
	_, body, _ = f.owner.get("/admin")
	mustContain(t, "suspended users", body, "@carol", "bots")
	code, body, _ = form(f.owner, "/admin", "/admin/users/suspend", url.Values{"login": {"alice"}})
	if code != http.StatusConflict {
		t.Fatalf("suspend admin: %d", code)
	}
	mustContain(t, "suspend admin", body, "Platform yöneticileri askıya alınamaz.")
	if code, _, _ := form(f.owner, "/admin", "/admin/users/suspend", url.Values{"login": {"ghost"}}); code != http.StatusNotFound {
		t.Fatalf("unknown user: %d", code)
	}

	// The legacy admin token is a platform admin too.
	f.ui.login()
	if code, _, _ := f.ui.get("/admin"); code != 200 {
		t.Fatalf("token admin: %d", code)
	}
}

// A suspended user's session ends; the login page says why.
func TestSuspendedSession(t *testing.T) {
	f := planUI(t, nil)
	carol, _ := f.st.UserByLogin(context.Background(), "carol")
	f.st.SuspendUser(context.Background(), carol.ID, "")
	code, _, h := f.viewer.get("/apps/blog")
	if code != http.StatusSeeOther || h.Get("Location") != "/login?error=suspended" {
		t.Fatalf("suspended session: %d %s", code, h.Get("Location"))
	}
	_, body, _ := f.viewer.get("/login?error=suspended")
	mustContain(t, "login page", body, "Hesabın bir yönetici tarafından askıya alındı.")
}

// Sign-in attempts and form posts are rate-limited per address and user.
func TestRateLimitsWeb(t *testing.T) {
	f := planUI(t, func(s *web.Server) {
		s.Limits = &ratelimit.Set{
			SignIn: ratelimit.New(ratelimit.Rule{Rate: 2, Per: time.Hour}, nil),
			Writes: ratelimit.New(ratelimit.Rule{Rate: 1, Per: time.Hour}, nil),
		}
	})
	for i := 0; i < 2; i++ {
		f.ui.post("/login", url.Values{"token": {"wrong"}}, "self")
	}
	code, body, h := f.ui.post("/login", url.Values{"token": {token}}, "self")
	if code != http.StatusTooManyRequests || h.Get("Retry-After") == "" {
		t.Fatalf("sign-in limit: %d", code)
	}
	mustContain(t, "sign-in limit", body, "Çok fazla giriş denemesi")

	// One write per hour for alice (and the test's single address).
	if code, _, _ := form(f.owner, "/apps/blog/settings", "/apps/blog/env", url.Values{"key": {"A"}, "value": {"1"}}); code == http.StatusTooManyRequests {
		t.Fatalf("first write limited")
	}
	code, body, _ = form(f.owner, "/apps/blog/settings", "/apps/blog/env", url.Values{"key": {"B"}, "value": {"2"}})
	if code != http.StatusTooManyRequests {
		t.Fatalf("second write: %d", code)
	}
	mustContain(t, "write limit", body, "Çok fazla istek")
	// Pages still load.
	if code, _, _ := f.owner.get("/apps/blog"); code != 200 {
		t.Fatalf("read after limit: %d", code)
	}
}

// The legal pages are public and linked from the footer and the login page.
func TestLegalPages(t *testing.T) {
	f := planUI(t, nil)
	anon := &ui{t: t, st: f.st, srv: f.ui.srv, client: newClient()}
	code, body, _ := anon.get("/terms")
	if code != 200 {
		t.Fatalf("terms: %d", code)
	}
	mustContain(t, "terms", body, "<h1>Kullanım koşulları</h1>", "Örnek Bulut", "Planlar ve limitler", "kvkk@example.com")
	code, body, _ = anon.get("/privacy")
	if code != 200 {
		t.Fatalf("privacy: %d", code)
	}
	mustContain(t, "privacy", body, "<h1>Gizlilik politikası</h1>", "KVKK", "GitHub kullanıcı adın", "şifreli",
		"Loglar", "Saklama süreleri", `href="mailto:kvkk@example.com"`)
	// The lawyer's note is a template comment, not page text.
	mustNotContain(t, "privacy", body, "avukat", "ŞABLON")
	mustNotContain(t, "terms", body, "avukat")

	_, body, _ = anon.get("/login")
	mustContain(t, "login", body, `href="/terms"`, `href="/privacy"`, "kabul edersin")
	_, body, _ = f.owner.get("/")
	mustContain(t, "footer", body, `<a href="/terms">Kullanım koşulları</a>`, `<a href="/privacy">Gizlilik politikası</a>`, `href="/usage"`)

	// Without a contact address the page says whom to ask.
	g := setupF20(t, nil)
	_, body, _ = (&ui{t: t, st: g.st, srv: g.ui.srv, client: newClient()}).get("/privacy")
	mustContain(t, "privacy without contact", body, "platform yöneticisine başvur")
}

// Open sign-up refuses GitHub accounts younger than the minimum age;
// admins and invited accounts still come in, suspended users do not.
func TestSignupGuards(t *testing.T) {
	u := setupOAuth(t)
	u.s.GitHub.OpenSignup = true
	u.s.GitHub.MinAccountAge = 30 * 24 * time.Hour
	ctx := context.Background()
	u.gh.mu.Lock()
	u.gh.created["stranger"] = time.Now().Add(-48 * time.Hour)
	u.gh.created["friend"] = time.Now().Add(-400 * 24 * time.Hour)
	u.gh.created["root"] = time.Now().Add(-time.Hour)
	u.gh.created["orgie"] = time.Now().Add(-time.Hour)
	u.gh.mu.Unlock()

	code, body := u.signIn("stranger")
	if code != http.StatusForbidden {
		t.Fatalf("new account: %d", code)
	}
	mustContain(t, "new account", body, "Bu GitHub hesabı yeni kayıt için çok yeni.")
	if _, err := u.st.UserByLogin(ctx, "stranger"); err == nil {
		t.Fatal("new account was stored")
	}
	if code, body := u.signIn("friend"); code != http.StatusOK {
		t.Fatalf("old account: %d\n%s", code, body)
	}
	friend, _ := u.st.UserByLogin(ctx, "friend")
	teams, _ := u.st.TeamsForUser(ctx, friend.ID)
	if len(teams) != 1 {
		t.Fatalf("personal team: %+v", teams)
	}
	if tp, _ := u.st.GetTeamPlan(ctx, teams[0].ID); tp.Plan != store.PlanHobby {
		t.Fatalf("open sign-up team plan: %s", tp.Plan)
	}
	u.client = newClient()
	if code, body := u.signIn("root"); code != http.StatusOK {
		t.Fatalf("new admin account: %d\n%s", code, body)
	}

	// An account a team owner invited comes in although it is new.
	orgie, _ := u.st.UpsertUser(ctx, 2, "orgie", "", "")
	u.st.SetMember(ctx, teams[0].ID, orgie.ID, store.RoleMember)
	u.client = newClient()
	if code, body := u.signIn("orgie"); code != http.StatusOK {
		t.Fatalf("invited new account: %d\n%s", code, body)
	}

	// A suspended user cannot sign in.
	u.st.SuspendUser(ctx, friend.ID, "")
	u.client = newClient()
	code, body = u.signIn("friend")
	if code != http.StatusForbidden {
		t.Fatalf("suspended user: %d", code)
	}
	mustContain(t, "suspended", body, "askıya alındı")
}
