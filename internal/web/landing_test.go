package web_test

import (
	"bytes"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/store"
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
		"saniyeler içinde canlıda</title>", `<meta name="description"`,
		`id="hero-title"`, `id="nasil-calisir"`, `id="ozellikler"`, `id="neden"`, `id="mimari"`, `id="planlar"`, `id="sss"`,
		`href="#nasil-calisir"`, `href="#ozellikler"`, `href="#planlar"`, // header section links
		`href="/login"`, `https://blog.paas.test`, `yeni-tasarim-blog.paas.test`, // the configured domain
		`src="/static/mimari.svg"`, `docs/MIMARI.md`, "Kayıtlar şu an davetle",
		"Bu sunucuda plan limiti uygulanmıyor", `href="/terms"`, `href="/privacy"`, `https://github.com/Nisagwn/paas"`,
		"<details", "Kendi sunucuma kurabilir miyim?")
	mustNotContain(t, "landing (signup closed)", body, `href="/signup"`, "/logout", "Yeni proje", "og:url")

	// CSP: every script carries the request's nonce; no inline handlers, no external images or styles.
	m := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(h.Get("Content-Security-Policy"))
	if m == nil {
		t.Fatal("no nonce in CSP")
	}
	tags := scriptTag.FindAllString(body, -1)
	if len(tags) == 0 {
		t.Fatal("no script tags")
	}
	for _, tag := range tags {
		if !strings.Contains(html.UnescapeString(tag), `nonce="`+m[1]+`"`) {
			t.Errorf("script without the request nonce: %s", tag)
		}
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
	mustNotContain(t, "terms", body, `href="#planlar"`, `href="/signup"`)
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

func TestLandingPlansAndOpenSignup(t *testing.T) {
	catalog := plans.DefaultCatalog()
	hobby := catalog[store.PlanHobby]
	hobby.Projects, hobby.BuildMinutes, hobby.Memory = 7, 1234, "3Gi"
	catalog[store.PlanHobby] = hobby
	pro := catalog[store.PlanPro]
	pro.DeploymentsPerDay = 4321
	catalog[store.PlanPro] = pro

	u := landingServer(t, func(s *web.Server) {
		s.Plans = &plans.Guard{Store: s.Store, Catalog: catalog}
		s.PublicURL = "https://paas.example.com/"
		s.GitHub = &auth.GitHubLogin{App: &github.OAuthApp{ClientID: "cid"}, OpenSignup: true}
	})
	code, body, _ := u.get("/")
	if code != http.StatusOK {
		t.Fatalf("GET /: %d", code)
	}
	mustContain(t, "landing (open signup)", body,
		`<a class="btn small primary" href="/signup">Kaydol</a>`, `<a class="btn small" href="/login">Giriş yap</a>`,
		`href="/signup">Ücretsiz başla`,
		`<meta property="og:url" content="https://paas.example.com/">`, `<link rel="canonical" href="https://paas.example.com/">`,
		`id="plan-hobby"`, `id="plan-pro"`, `id="plan-unlimited"`, ">Hobby<", ">Pro<", ">Sınırsız<",
		"<dt>Proje</dt><dd>7</dd>", "<dd>1.234 dk</dd>", "<dd>3 GiB</dd>", "<dt>Günlük deploy</dt><dd>4.321</dd>",
		"<dt>Proje</dt><dd>Sınırsız</dd>")
	mustNotContain(t, "landing (open signup)", body, "<dt>Proje</dt><dd>10</dd>")
	mustNotContain(t, "landing (open signup)", body, "Kayıtlar şu an davetle", "plan limiti uygulanmıyor")

	// The same plans with sign-up closed point to the login page.
	u = landingServer(t, func(s *web.Server) {
		s.Plans = &plans.Guard{Store: s.Store, Catalog: catalog}
		s.GitHub = &auth.GitHubLogin{App: &github.OAuthApp{ClientID: "cid"}}
	})
	_, body, _ = u.get("/")
	mustContain(t, "landing (invite only)", body, "Kayıtlar şu an davetle", "<dd>1.234 dk</dd>",
		`<a class="btn small primary" href="/login">Giriş yap</a>`, `href="/login">Giriş yap<span class="sr-only"> (Hobby planı)`)
	mustNotContain(t, "landing (invite only)", body, `href="/signup"`)
}

func TestArchitectureImage(t *testing.T) {
	u := setup(t)
	code, body, h := u.get("/static/mimari.svg")
	if code != http.StatusOK || h.Get("Content-Type") != "image/svg+xml" || !strings.Contains(body, "<svg") {
		t.Fatalf("GET /static/mimari.svg: %d %q", code, h.Get("Content-Type"))
	}
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("image without nosniff")
	}
}

// TestArchitectureImageInSync: the embedded copy matches the documentation's image.
func TestArchitectureImageInSync(t *testing.T) {
	doc, err := os.ReadFile("../../docs/images/mimari-genel.svg")
	if err != nil {
		t.Skip("docs not present:", err)
	}
	embedded, err := os.ReadFile("static/mimari.svg")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(doc, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(embedded, []byte("\r\n"), []byte("\n"))) {
		t.Fatal("internal/web/static/mimari.svg differs from docs/images/mimari-genel.svg; copy it again")
	}
}
