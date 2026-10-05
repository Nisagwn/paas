package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/domains"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/web"
)

func setupDomains(t *testing.T) *ui {
	st := testdb.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &web.Server{
		Store: st, Sessions: auth.New(token), Domain: "paas.test", Log: log,
		Domains: &domains.Verifier{Store: st, Domain: "paas.test", Mode: domains.ModeSkip, Log: log},
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return &ui{t: t, st: st, srv: srv, client: client}
}

func TestDomainForms(t *testing.T) {
	u := setupDomains(t)
	u.login()
	ctx := context.Background()
	app, _ := u.st.CreateApp(ctx, "blog", "nisagwn/blog", "main")

	code, body, _ := u.get("/apps/blog/settings")
	if code != 200 || !strings.Contains(body, "Alan adları") || !strings.Contains(body, "Henüz alan adı yok.") {
		t.Fatalf("app page: %d", code)
	}
	csrf := u.csrf("/apps/blog/settings")

	// CSRF and same-origin are required.
	if code, _, _ := u.post("/apps/blog/domains", url.Values{"hostname": {"www.example.com"}}, "self"); code != http.StatusForbidden {
		t.Fatalf("add without CSRF: %d", code)
	}
	if code, _, _ := u.post("/apps/blog/domains", url.Values{"hostname": {"www.example.com"}, "csrf": {csrf}}, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin add: %d", code)
	}

	// Invalid hostnames are explained on the page.
	code, body, _ = u.post("/apps/blog/domains", url.Values{"hostname": {"x.paas.test"}, "csrf": {csrf}}, "self")
	if code != http.StatusBadRequest || !strings.Contains(body, "platforma ait") {
		t.Fatalf("platform hostname: %d", code)
	}

	code, _, h := u.post("/apps/blog/domains", url.Values{"hostname": {"www.example.com"}, "csrf": {csrf}}, "self")
	if code != http.StatusSeeOther || h.Get("Location") != "/apps/blog/settings?ok=domain#custom-domains" {
		t.Fatalf("add: %d %q", code, h.Get("Location"))
	}
	d, err := u.st.GetDomain(ctx, app.ID, "www.example.com")
	if err != nil || d.Status != store.DomainVerified || !d.Routed {
		t.Fatalf("stored domain: %+v %v", d, err)
	}

	// The page lists it with its status and the DNS records to create.
	_, body, _ = u.get("/apps/blog/settings")
	for _, want := range []string{"www.example.com", "s-verified", "İlk canlı deploy bekleniyor",
		"_paas-challenge.www.example.com", d.Token, "blog.paas.test", "Alan adları güncellendi."} {
		if want == "Alan adları güncellendi." {
			_, body, _ = u.get("/apps/blog/settings?ok=domain")
		}
		if !strings.Contains(body, want) {
			t.Errorf("app page lacks %q", want)
		}
	}

	// htmx gets the fragment back.
	req, _ := http.NewRequest("POST", u.srv.URL+"/apps/blog/domains/verify",
		strings.NewReader(url.Values{"hostname": {"www.example.com"}, "csrf": {csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", u.srv.URL)
	req.Header.Set("HX-Request", "true")
	resp, err := u.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(strings.TrimSpace(string(b)), `<div id="domains-card"`) {
		t.Fatalf("htmx verify: %d\n%s", resp.StatusCode, b)
	}

	code, _, _ = u.post("/apps/blog/domains/delete", url.Values{"hostname": {"www.example.com"}, "csrf": {csrf}}, "self")
	if code != http.StatusSeeOther {
		t.Fatalf("delete: %d", code)
	}
	if _, err := u.st.GetDomain(ctx, app.ID, "www.example.com"); err == nil {
		t.Fatal("domain not deleted")
	}
	if code, _, _ := u.post("/apps/blog/domains/delete", url.Values{"hostname": {"www.example.com"}, "csrf": {csrf}}, "self"); code != http.StatusNotFound {
		t.Fatalf("delete missing: %d", code)
	}
}
