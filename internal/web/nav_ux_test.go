package web_test

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/store"
)

// Navigation and first use: header, dashboard cards, onboarding checklist,
// team switcher, error page, global helpers.

var scriptRe = regexp.MustCompile(`<script[^>]*>`)

// checkNonces fails when a <script> lacks the response's CSP nonce.
func checkNonces(t *testing.T, what, body string, h http.Header) {
	t.Helper()
	m := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(h.Get("Content-Security-Policy"))
	if m == nil {
		t.Fatalf("%s: no nonce in CSP %q", what, h.Get("Content-Security-Policy"))
	}
	tags := scriptRe.FindAllString(body, -1)
	if len(tags) == 0 {
		t.Fatalf("%s: no scripts", what)
	}
	for _, tag := range tags {
		if !strings.Contains(html.UnescapeString(tag), `nonce="`+m[1]+`"`) {
			t.Errorf("%s: script without the CSP nonce: %s", what, tag)
		}
	}
	if strings.Contains(body, " onclick=") || strings.Contains(body, " onsubmit=") {
		t.Errorf("%s: inline event handler (blocked by CSP)", what)
	}
}

func TestDashboardOnboardingAndCards(t *testing.T) {
	u := setup(t)
	u.login()
	ctx := context.Background()

	// A new user: welcome, the checklist, and the next step first.
	code, body, h := u.get("/")
	if code != 200 {
		t.Fatalf("dashboard: %d", code)
	}
	mustContain(t, "empty dashboard", body, "paas'a hoş geldin", "İlk adımlar", `class="checklist"`,
		`<li class="skip">`, `<li class="current" aria-current="step">`, `href="/import"`)
	checkNonces(t, "dashboard", body, h)
	// Header: skip link, highlighted section, primary action, user menu.
	mustContain(t, "header", body, `<a class="skip" href="#main">`, `<main id="main"`, `aria-label="Ana menü"`,
		`<a href="/" class="on" aria-current="page">Projeler</a>`, `<a class="btn primary small" href="/import">`,
		`href="/tokens"`, "Çıkış yap")

	// Two projects; blog goes live, shop has a failed preview.
	for _, a := range [][2]string{{"blog", "nisagwn/blog"}, {"shop", "nisagwn/shop"}} {
		form := url.Values{"name": {a[0]}, "repo": {a[1]}, "csrf": {u.csrf("/")}}
		if code, body, _ := u.post("/apps", form, "self"); code != http.StatusSeeOther {
			t.Fatalf("create %s: %d\n%s", a[0], code, body)
		}
	}
	_, body, _ = u.get("/")
	mustContain(t, "projects without deploys", body, `<li class="done">`, "Henüz deploy yok", "Canlı değil")

	blog, _ := u.st.GetAppByName(ctx, "blog")
	shop, _ := u.st.GetAppByName(ctx, "shop")
	d, _, _ := u.st.EnqueueDeployment(ctx, blog.ID, sha(1), "main", "Ana sayfa\n\nayrıntı")
	u.st.MarkReady(ctx, d, []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}})
	u.st.EnqueueDeployment(ctx, shop.ID, sha(2), "feature/cart", "Sepet")

	code, body, h = u.get("/")
	if code != 200 {
		t.Fatalf("dashboard: %d", code)
	}
	mustNotContain(t, "live dashboard", body, "İlk adımlar")
	mustContain(t, "cards", body,
		`<a class="name stretch" href="/apps/blog">blog</a>`,
		`data-copy="https://blog.paas.test"`, // copy button of the live URL
		`<span class="badge s-ready" title="Production yayında">Canlı</span>`,
		">Ana sayfa<", "⎇ main", `<time datetime="`, `data-ago>`, `title="`, // relative time with exact tooltip
		"⎇ feature/cart", `hx-trigger="every 5s"`, // shop's deploy is still queued
		`data-search data-filter="#projects"`, "2 proje")
	checkNonces(t, "dashboard", body, h)

	// Search works without JavaScript (?q=).
	_, body, _ = u.get("/?q=sho")
	mustContain(t, "search", body, `href="/apps/shop"`, `value="sho"`)
	mustNotContain(t, "search", body, `href="/apps/blog"`)
	_, body, _ = u.get("/?q=zzz")
	mustContain(t, "empty search", body, `id="projects-empty">`, "Aramana uyan proje yok")
}

func TestTeamSwitcher(t *testing.T) {
	u := setup(t)
	u.login()
	ctx := context.Background()
	owner, _ := u.st.UpsertUser(ctx, 9, "ayse", "Ayşe", "")
	web, err := u.st.CreateTeam(ctx, "web", "Web Ekibi", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	def, _ := u.st.GetTeamBySlug(ctx, store.DefaultTeam)
	u.st.CreateAppInTeam(ctx, web.ID, "site", "acme/site", "main")
	u.st.CreateAppInTeam(ctx, def.ID, "blog", "acme/blog", "main")

	_, body, _ := u.get("/")
	mustContain(t, "switcher", body, `class="dropdown switcher"`, "Tüm ekipler", `href="/?team=web"`, "Web Ekibi",
		`href="/apps/site"`, `href="/apps/blog"`)

	code, body, h := u.get("/?team=web")
	if code != 200 || !strings.Contains(h.Get("Set-Cookie"), "paas_team=web") {
		t.Fatalf("select team: %d %q", code, h.Get("Set-Cookie"))
	}
	mustContain(t, "selected team", body, `href="/apps/site"`, "Web Ekibi ekibinin projeleri", `href="/?team=web" aria-current="true"`)
	mustNotContain(t, "selected team", body, `class="name stretch" href="/apps/blog"`)
	// The choice is remembered.
	_, body, _ = u.get("/")
	mustNotContain(t, "remembered team", body, `class="name stretch" href="/apps/blog"`)
	// "Tüm ekipler" clears it.
	_, body, h = u.get("/?team=")
	if !strings.Contains(h.Get("Set-Cookie"), "paas_team=;") {
		t.Errorf("clear team cookie: %q", h.Get("Set-Cookie"))
	}
	mustContain(t, "all teams", body, `href="/apps/blog"`, `href="/apps/site"`)

	// Other sections are highlighted in the header.
	_, body, _ = u.get("/teams")
	mustContain(t, "teams page", body, `<a href="/teams" class="on" aria-current="page">Ekipler</a>`)
	// Team pages share the teamtabs and crumbs helpers.
	_, body, _ = u.get("/teams/web")
	mustContain(t, "team page", body, `<nav class="tabs tabs-spaced" aria-label="Ekip bölümleri">`,
		`<a class="on" aria-current="page" href="/teams/web">Genel</a>`, `href="/usage?team=web"`,
		`<nav class="crumbs" aria-label="Konum">`, `<span aria-current="page">Web Ekibi</span>`)
}

func TestErrorAndLoginPages(t *testing.T) {
	u := setup(t)
	code, body, h := u.get("/login")
	if code != 200 {
		t.Fatalf("login: %d", code)
	}
	mustContain(t, "login", body, "Token ile giriş yap", `autocomplete="current-password"`, `<label for="token">`)
	mustNotContain(t, "login", body, `aria-label="Ana menü"`)
	checkNonces(t, "login", body, h)
	if code, body, _ := u.post("/login", url.Values{"token": {"wrong"}}, "self"); code != http.StatusUnauthorized ||
		strings.Count(body, "Geçersiz API token") != 1 {
		t.Fatalf("wrong token shown once inside the card: %d", code)
	}

	u.login()
	code, body, h = u.get("/does/not/exist")
	if code != http.StatusNotFound {
		t.Fatalf("404: %d", code)
	}
	mustContain(t, "404", body, `<p class="code" aria-hidden="true">404</p>`, "Bulunamadı", "Sayfa bulunamadı.",
		"taşınmış", `href="/">Projelere dön`, "data-back")
	mustNotContain(t, "404", body, `class="flash err"`) // the message is shown once, in the card
	checkNonces(t, "404", body, h)
}

func TestImportManualAndErrors(t *testing.T) {
	u := setup(t)
	u.login()
	_, body, _ := u.get("/import")
	mustContain(t, "import (no GitHub App)", body, `<nav class="crumbs" aria-label="Konum">`,
		`<span aria-current="page">Yeni proje</span>`, `id="manual" open`, `action="/apps"`)

	// A failed manual add comes back to the import page with the values kept.
	form := url.Values{"name": {"Bad Name"}, "repo": {"nisagwn/blog"}, "csrf": {u.csrf("/import")}}
	code, body, _ := u.post("/apps", form, "self")
	if code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", code)
	}
	mustContain(t, "bad name", body, "Ad 2-31 karakter", `value="nisagwn/blog"`, `id="manual" open`, `role="alert"`)

	// Duplicate: an explanation and what to do.
	form.Set("name", "blog")
	u.post("/apps", form, "self")
	code, body, _ = u.post("/apps", form, "self")
	if code != http.StatusConflict {
		t.Fatalf("duplicate: %d", code)
	}
	mustContain(t, "duplicate", body, "zaten var", "farklı bir ad dene")

	// An unknown repository in step 3 falls back to the list with help.
	_, body, _ = u.get("/import?repo=acme/none")
	mustContain(t, "unknown repo", body, "içe aktarılabilecek repolar arasında yok")
}
