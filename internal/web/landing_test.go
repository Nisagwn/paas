package web_test

import (
	"bytes"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/web"
)

// landingServer serves a UI configured by fn (open signup, plans, public URL).
func landingServer(t *testing.T, fn func(*web.Server)) *ui {
	st := testdb.Open(t)
	s := &web.Server{
		Store: st, Sessions: auth.New(token), Domain: "paas.test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if fn != nil {
		fn(s)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &ui{t: t, st: st, srv: srv, client: newClient()}
}

var (
	scriptTag = regexp.MustCompile(`<script\b[^>]*>`)
	inlineOn  = regexp.MustCompile(`(?i)<[^>]+\son[a-z]+\s*=`)
	extAsset  = regexp.MustCompile(`(?i)<(img|link|source)\b[^>]*(src|href)="https?://`)
)

func TestLandingAnonymous(t *testing.T) {
	u := setup(t) // no GitHub login: sign-up is closed, no plans enforced
	code, body, h := u.get("/")
	if code != http.StatusOK {
		t.Fatalf("GET /: %d, want the landing page", code)
	}
	mustContain(t, "landing", body,
		"saniyeler içinde canlıda</title>", `<meta name="description"`, `<meta property="og:title"`,
		`id="hero-title"`, `id="nasil-calisir"`, `id="ozellikler"`, `id="proof-title"`, `id="final-title"`,
		`href="#nasil-calisir">Nasıl çalışır</a>`, `href="#ozellikler">Özellikler</a>`, // header section links
		`<a class="btn small primary" href="/login">Giriş yap</a>`,
		`<a class="btn primary big" href="/login">Giriş yap`, "Kayıtlar şu an davetle.",
		`https://blog.paas.test`, `yeni-tasarim-blog.paas.test`, // the configured domain
		`docs/MIMARI.md`, `docs/MEASUREMENTS.md`, "Bu sunucuda plan limiti uygulanmıyor",
		`href="/terms"`, `href="/privacy"`, `https://github.com/Nisagwn/paas"`,
		`<figure class="term" data-play`, `aria-hidden="true"><svg`)
	mustNotContain(t, "landing (signup closed)", body, `href="/signup"`, "/logout", "Yeni proje", "og:url",
		`href="#planlar"`, "/static/mimari.svg", "<details", "<table")

	// Exactly one h1; five sections at most; six feature cards; two header links at most.
	if n := strings.Count(body, "<h1"); n != 1 {
		t.Errorf("%d h1 elements, want 1", n)
	}
	if n := strings.Count(body, "<section"); n > 5 {
		t.Errorf("%d sections, want at most 5", n)
	}
	feat := body[strings.Index(body, `<ul class="lp-feat">`):]
	if n := strings.Count(feat[:strings.Index(feat, "</ul>")], "<li"); n != 6 {
		t.Errorf("%d feature cards, want 6", n)
	}
	nav := body[strings.Index(body, `aria-label="Sayfa bölümleri"`):]
	if n := strings.Count(nav[:strings.Index(nav, "</nav>")], "<a "); n > 2 {
		t.Errorf("%d header section links, want at most 2", n)
	}

	// CSP: every script carries the request's nonce; no inline handlers, no external images or styles.
	m := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(h.Get("Content-Security-Policy"))
	if m == nil {
		t.Fatal("no nonce in CSP")
	}
	tags := scriptTag.FindAllString(body, -1)
	if len(tags) < 3 { // htmx, the layout script, the terminal player
		t.Fatalf("%d script tags, want the landing script too", len(tags))
	}
	for _, tag := range tags {
		if !strings.Contains(html.UnescapeString(tag), `nonce="`+m[1]+`"`) {
			t.Errorf("script without the request nonce: %s", tag)
		}
	}
	if !strings.Contains(body, `matchMedia("(prefers-reduced-motion: reduce)")`) {
		t.Error("terminal player ignores prefers-reduced-motion")
	}
	if s := inlineOn.FindString(body); s != "" {
		t.Errorf("inline event handler: %s", s)
	}
	if s := extAsset.FindString(body); s != "" {
		t.Errorf("external asset: %s", s)
	}
	for _, word := range []string{"mini", "basit", "MVP", "küçük", "yeterli", "tek kişi", "bitirme"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(word)) {
			t.Errorf("landing uses %q", word)
		}
	}

	// Other public pages keep the signed-out header without the section links.
	_, body, _ = u.get("/terms")
	mustContain(t, "terms", body, `href="/login"`)
	mustNotContain(t, "terms", body, `href="#ozellikler"`, `href="/signup"`)

	// The architecture image lives in the documentation only.
	if code, _, h := u.get("/static/mimari.svg"); code == http.StatusOK || h.Get("Content-Type") == "image/svg+xml" {
		t.Errorf("GET /static/mimari.svg: %d %q, want no image", code, h.Get("Content-Type"))
	}
}

func TestLandingSignedIn(t *testing.T) {
	u := setup(t)
	u.login()
	code, body, _ := u.get("/")
	if code != http.StatusOK {
		t.Fatalf("GET /: %d", code)
	}
	mustContain(t, "dashboard", body, "/logout", `href="/import"`)
	mustNotContain(t, "dashboard", body, `id="hero-title"`, `href="/signup"`)

	// A bearer token still reaches the dashboard, never the landing page.
	req, _ := http.NewRequest("GET", u.srv.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || bytes.Contains(b, []byte(`id="hero-title"`)) {
		t.Fatalf("bearer GET /: %d, landing=%v", resp.StatusCode, bytes.Contains(b, []byte(`id="hero-title"`)))
	}
	// Other pages still send anonymous visitors to the login page.
	if code, _, h := newUI(u).get("/apps/blog"); code != http.StatusSeeOther || !strings.HasPrefix(h.Get("Location"), "/login") {
		t.Fatalf("anonymous /apps/blog: %d %q", code, h.Get("Location"))
	}
}

// newUI is another browser (empty cookie jar) on the same server.
func newUI(u *ui) *ui { return &ui{t: u.t, st: u.st, srv: u.srv, client: newClient()} }

func TestLandingOpenSignup(t *testing.T) {
	u := landingServer(t, func(s *web.Server) {
		s.Plans = &plans.Guard{Store: s.Store, Catalog: plans.DefaultCatalog()}
		s.PublicURL = "https://paas.example.com/"
		s.GitHub = &auth.GitHubLogin{App: &github.OAuthApp{ClientID: "cid"}, OpenSignup: true}
	})
	code, body, _ := u.get("/")
	if code != http.StatusOK {
		t.Fatalf("GET /: %d", code)
	}
	mustContain(t, "landing (open signup)", body,
		`<a class="btn small primary" href="/signup">Kaydol</a>`, `<a class="btn small" href="/login">Giriş yap</a>`,
		`<a class="btn primary big" href="/signup">Ücretsiz başla`, `<a class="btn big" href="/login">Giriş yap</a>`,
		`Hobby planı ücretsiz · <a href="/signup">Limitleri gör`,
		`<meta property="og:url" content="https://paas.example.com/">`, `<link rel="canonical" href="https://paas.example.com/">`)
	mustNotContain(t, "landing (open signup)", body, "Kayıtlar şu an davetle", "plan limiti uygulanmıyor")

	// The limits the landing page links to are listed on /signup.
	_, body, _ = u.get("/signup")
	mustContain(t, "signup", body, "Hobby planı limitleri")

	// Plans with sign-up closed point to the login page.
	u = landingServer(t, func(s *web.Server) {
		s.Plans = &plans.Guard{Store: s.Store, Catalog: plans.DefaultCatalog()}
		s.GitHub = &auth.GitHubLogin{App: &github.OAuthApp{ClientID: "cid"}}
	})
	_, body, _ = u.get("/")
	mustContain(t, "landing (invite only)", body, "Kayıtlar şu an davetle", "Hobby planı ücretsiz.",
		`<a class="btn small primary" href="/login">Giriş yap</a>`, `<a class="btn primary big" href="/login">Giriş yap`)
	mustNotContain(t, "landing (invite only)", body, `href="/signup"`)
}
