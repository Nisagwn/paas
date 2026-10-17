package web_test

import (
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/web"
)

var githubHrefRe = regexp.MustCompile(`href="(/auth/github\?next=[^"]*)"`)

// The GitHub button's return path, unescaped.
func githubNext(t *testing.T, body string) string {
	t.Helper()
	m := githubHrefRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no GitHub button:\n%s", body)
	}
	u, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("next")
}

// Open sign-up: the plan limits come from the configured catalog, a new
// account is sent to the import page, the flow is the GitHub login.
func TestSignupOpen(t *testing.T) {
	u := setupOAuth(t)
	u.s.GitHub.OpenSignup = true
	u.s.GitHub.MinAccountAge = 30 * 24 * time.Hour
	cat := plans.DefaultCatalog()
	h := cat[store.PlanHobby]
	h.Projects, h.BuildMinutes, h.DeploymentsPerDay, h.Domains, h.Addons = 7, 1234, 42, plans.Unlimited, 5
	cat[store.PlanHobby] = h
	u.s.Plans = &plans.Guard{Store: u.st, Catalog: cat}

	code, body, hdr := u.get("/signup")
	if code != 200 {
		t.Fatalf("signup: %d", code)
	}
	mustContain(t, "signup", body, "<title>Kaydol · paas</title>", "<h1>Hesap oluştur</h1>", "GitHub ile kaydol",
		"Hobby planı", "<li>7 proje</li>", "<li>ayda 1.234 build dakikası</li>", "<li>günde 42 deploy</li>",
		"<li>sınırsız özel alan adı</li>", "<li>5 veritabanı</li>", "en az 30 gün önce",
		"Ne olacak?", "GitHub hesabınla doğrulanırsın", "Repo erişimini sonra sen seçersin", "GitHub App'i kurar",
		"Kodun yalnızca build için çekilir", "kısa ömürlü kurulum token", "read:org",
		`href="/terms"`, `href="/privacy"`, "Kaydolarak", "Zaten hesabın var mı?", `href="/login">Giriş yap</a>`,
		`class="auth-brand" href="/"`)
	mustNotContain(t, "signup", body, "davetle", `name="token"`, "<li>10 proje</li>")
	checkNonces(t, "signup", body, hdr)
	if next := githubNext(t, body); next != "/import" {
		t.Fatalf("signup return path = %q", next)
	}

	// The button runs the same GitHub authorization; the new user lands on /import.
	code, _, hdr = u.get("/auth/github?next=/import")
	if code != http.StatusFound {
		t.Fatalf("start: %d", code)
	}
	q := mustLocation(t, hdr).Query()
	u.gh.mu.Lock()
	u.gh.challenge = q.Get("code_challenge")
	u.gh.created["friend"] = time.Now().Add(-400 * 24 * time.Hour)
	u.gh.mu.Unlock()
	code, body, _ = u.get("/auth/github/callback?code=friend&state=" + url.QueryEscape(q.Get("state")))
	if code != 200 || !strings.Contains(body, "url=/import") {
		t.Fatalf("signup callback: %d\n%s", code, body)
	}

	// Signed in: both pages send the user on.
	for _, p := range []string{"/signup", "/login"} {
		if code, _, hdr := u.get(p); code != http.StatusSeeOther || hdr.Get("Location") != "/" {
			t.Errorf("%s signed in: %d → %q", p, code, hdr.Get("Location"))
		}
	}

	// Without plan enforcement no limits are shown.
	u.s.Plans = nil
	_, body, _ = (&ui{t: t, st: u.st, srv: u.srv, client: newClient()}).get("/signup")
	mustNotContain(t, "signup without plans", body, "Hobby planı", "proje</li>")
}

func mustLocation(t *testing.T, h http.Header) *url.URL {
	t.Helper()
	loc, err := url.Parse(h.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// Invite-only: the page explains invitations and offers the sign-in.
func TestSignupInviteOnly(t *testing.T) {
	u := setupOAuth(t)
	code, body, _ := u.get("/signup")
	if code != 200 {
		t.Fatalf("signup: %d", code)
	}
	mustContain(t, "invite-only", body, "<h1>Kayıtlar şu an davetle</h1>", "ekip sahibi GitHub kullanıcı adını",
		"GitHub ile giriş yap", "kuruluşlarının üyeleri", "Ne olacak?", `href="/login">Giriş yap</a>`)
	mustNotContain(t, "invite-only", body, "GitHub ile kaydol", "planı", "acme")
	if next := githubNext(t, body); next != "/" {
		t.Fatalf("invited return path = %q", next)
	}
}

// Without GitHub login (dev mode) the page points to the token form.
func TestSignupDevMode(t *testing.T) {
	u := setup(t)
	code, body, h := u.get("/signup")
	if code != 200 {
		t.Fatalf("signup: %d", code)
	}
	mustContain(t, "dev signup", body, "Geliştirme modu", "PAAS_API_TOKEN", "GitHub App kurulumu", `href="/login"`)
	mustNotContain(t, "dev signup", body, "/auth/github", "Ne olacak?")
	checkNonces(t, "dev signup", body, h)

	// The token form is open in dev mode.
	_, body, _ = u.get("/login")
	mustContain(t, "dev login", body, "<h1>Tekrar hoş geldin</h1>", `<details class="auth-token" open>`,
		"Yönetici token'ı ile giriş", `<label for="token">`, "autofocus", `href="/signup">Kaydol</a>`)
	mustNotContain(t, "dev login", body, "/auth/github")
}

// The login page: the token form is folded under GitHub, hidden without
// token login; errors say what to do next.
func TestLoginPageVariants(t *testing.T) {
	u := setupOAuth(t)
	u.s.GitHub.OpenSignup = true
	u.s.GitHub.MinAccountAge = 14 * 24 * time.Hour
	code, body, h := u.get("/login")
	if code != 200 {
		t.Fatalf("login: %d", code)
	}
	mustContain(t, "oauth login", body, "<title>Giriş yap · paas</title>", "GitHub ile giriş yap", "Hesabın yok mu?", `href="/signup">Kaydol</a>`)
	mustNotContain(t, "oauth login", body, `name="token"`, "Yönetici token", `class="flash err"`)
	checkNonces(t, "oauth login", body, h)

	for _, c := range []struct{ code, want, help string }{
		{"not_allowed", "giriş izni yok", `Davet nasıl işler →`},
		{"suspended", "askıya alındı", `href="/terms"`},
		{"too_new", "çok yeni", "en az 14 gün önce"},
		{"state", "bu tarayıcıda başlatılmadı", `Tekrar dene →`},
		{"denied", "iptal edildi", `Tekrar dene →`},
	} {
		_, body, _ := u.get("/login?error=" + c.code)
		mustContain(t, c.code, body, `aria-live="assertive"`, `role="alert"`, c.want, `class="help"`, c.help)
	}
	_, body, _ = u.get("/login?error=<script>")
	mustNotContain(t, "unknown error", body, `class="flash err"`, "&lt;script")
	// A forged state ends on the login page with a retry link.
	code, body, _ = u.get("/auth/github/callback?code=root&state=forged")
	if code != http.StatusBadRequest {
		t.Fatalf("forged state: %d", code)
	}
	mustContain(t, "forged state", body, "Tekrar dene →", `href="/auth/github?next=`)

	// GitHub and token login both on: the token form starts folded.
	st := testdb.Open(t)
	s := &web.Server{Store: st, Sessions: auth.New(token), Domain: "paas.test",
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		GitHub: &auth.GitHubLogin{App: &github.OAuthApp{ClientID: "cid", WebURL: "https://github.invalid"}, RedirectURI: "https://x/cb"}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	both := &ui{t: t, st: st, srv: srv, client: newClient()}
	_, body, _ = both.get("/login")
	mustContain(t, "both", body, "GitHub ile giriş yap", `<details class="auth-token">`, `name="token"`, `<div class="divider">ya da</div>`)
	mustNotContain(t, "both", body, "autofocus")
	// A wrong token opens it again with the error.
	_, body, _ = both.post("/login", url.Values{"token": {"wrong"}}, "self")
	mustContain(t, "both wrong token", body, `<details class="auth-token" open>`, "Geçersiz API token", "PAAS_API_TOKEN değerinden")
}

// ?next= only accepts same-site relative paths, on every entry point.
func TestNextOpenRedirect(t *testing.T) {
	u := setup(t)
	bad := []string{"https://evil.example/", "//evil.example", "/\\evil.example", "/\t/evil.example", "/%2F/evil.example",
		"javascript:alert(1)", "evil.example", "/\n/evil.example", "\\\\evil.example"}
	for _, next := range bad {
		if _, body, _ := u.get("/login?next=" + url.QueryEscape(next)); !strings.Contains(body, `name="next" value="/"`) {
			t.Errorf("login form keeps next %q", next)
		}
		if _, body, _ := u.get("/signup?next=" + url.QueryEscape(next)); strings.Contains(body, "evil") {
			t.Errorf("signup reflects next %q", next)
		}
	}
	u.login()
	for _, next := range bad {
		if code, _, h := u.get("/login?next=" + url.QueryEscape(next)); code != http.StatusSeeOther || h.Get("Location") != "/" {
			t.Errorf("signed-in /login?next=%q → %d %q", next, code, h.Get("Location"))
		}
		if code, _, h := u.get("/signup?next=" + url.QueryEscape(next)); code != http.StatusSeeOther || h.Get("Location") != "/" {
			t.Errorf("signed-in /signup?next=%q → %d %q", next, code, h.Get("Location"))
		}
	}
	if code, _, h := u.get("/login?next=" + url.QueryEscape("/apps/blog?tab=env#x")); code != http.StatusSeeOther || h.Get("Location") != "/apps/blog?tab=env#x" {
		t.Errorf("local next: %d %q", code, h.Get("Location"))
	}

	// The OAuth start keeps only a safe return path through GitHub.
	o := setupOAuth(t)
	code, _, h := o.get("/auth/github?next=" + url.QueryEscape("//evil.example/x"))
	if code != http.StatusFound {
		t.Fatalf("start: %d", code)
	}
	q := mustLocation(t, h).Query()
	o.gh.mu.Lock()
	o.gh.challenge = q.Get("code_challenge")
	o.gh.mu.Unlock()
	code, body, _ := o.get("/auth/github/callback?code=root&state=" + url.QueryEscape(q.Get("state")))
	if code != 200 || strings.Contains(body, "evil") || !strings.Contains(body, `url=/"`) {
		t.Fatalf("callback next: %d\n%s", code, body)
	}
}
