package web_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/web"
)

const token = "ui-token"

type ui struct {
	t      *testing.T
	st     *store.Store
	srv    *httptest.Server
	client *http.Client
}

func setup(t *testing.T) *ui {
	st := testdb.Open(t)
	s := &web.Server{
		Store: st, Sessions: auth.New(token), Domain: "paas.test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // inspect redirects instead of following them
	}}
	return &ui{t: t, st: st, srv: srv, client: client}
}

func (u *ui) get(path string) (int, string, http.Header) {
	u.t.Helper()
	resp, err := u.client.Get(u.srv.URL + path)
	if err != nil {
		u.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// post submits a form; origin "" sends none, "self" sends the server's own.
func (u *ui) post(path string, form url.Values, origin string) (int, string, http.Header) {
	u.t.Helper()
	req, _ := http.NewRequest("POST", u.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin == "self" {
		origin = u.srv.URL
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// csrf scrapes the form token from a rendered page.
func (u *ui) csrf(path string) string {
	u.t.Helper()
	_, body, _ := u.get(path)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		u.t.Fatalf("no CSRF token on %s", path)
	}
	return m[1]
}

func (u *ui) login() {
	u.t.Helper()
	code, _, h := u.post("/login", url.Values{"token": {token}, "next": {"/"}}, "self")
	if code != http.StatusSeeOther || h.Get("Location") != "/" {
		u.t.Fatalf("login: %d → %q", code, h.Get("Location"))
	}
}

func sha(n int) string { return fmt.Sprintf("%040x", n) }

func TestLoginRequired(t *testing.T) {
	u := setup(t)
	code, _, h := u.get("/apps/blog")
	if code != http.StatusSeeOther || h.Get("Location") != "/login?next=%2Fapps%2Fblog" {
		t.Fatalf("anonymous page: %d → %q", code, h.Get("Location"))
	}
	if code, _, _ := u.post("/apps", url.Values{"name": {"x"}}, "self"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous POST: %d, want 401", code)
	}
	if code, body, _ := u.post("/login", url.Values{"token": {"wrong"}}, "self"); code != http.StatusUnauthorized ||
		!strings.Contains(body, "Geçersiz API token") {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _, _ := u.post("/login", url.Values{"token": {token}}, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin login: %d, want 403", code)
	}
	// ?next= never leaves the site.
	code, _, h = u.post("/login", url.Values{"token": {token}, "next": {"//evil.example/x"}}, "self")
	if code != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("open redirect: %d → %q", code, h.Get("Location"))
	}

	// Scripts may use the bearer token instead of a session.
	req, _ := http.NewRequest("GET", u.srv.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer GET: %v %v", resp, err)
	}
	resp.Body.Close()
	if h := resp.Header.Get("Content-Security-Policy"); !strings.Contains(h, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", h)
	}
}

func TestPagesAndRollback(t *testing.T) {
	u := setup(t)
	u.login()
	ctx := context.Background()

	// Create an app through the form.
	form := url.Values{"name": {"blog"}, "repo": {"nisagwn/blog"}, "csrf": {u.csrf("/")}}
	if code, body, _ := u.post("/apps", form, "self"); code != http.StatusSeeOther {
		t.Fatalf("create app: %d\n%s", code, body)
	}
	app, err := u.st.GetAppByName(ctx, "blog")
	if err != nil {
		t.Fatal(err)
	}

	// Two ready deployments on main; production follows the second.
	prod := []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}}
	d1, _, _ := u.st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "first commit")
	d2, _, _ := u.st.EnqueueDeployment(ctx, app.ID, sha(2), "main", "second commit")
	u.st.MarkReady(ctx, d1, prod)
	u.st.MarkReady(ctx, d2, prod)
	u.st.AppendLog(ctx, d1.ID, "==> hello from the build log")

	if code, body, _ := u.get("/"); code != 200 || !strings.Contains(body, "blog") || !strings.Contains(body, "nisagwn/blog") {
		t.Fatalf("apps page: %d\n%s", code, body)
	}
	code, body, _ := u.get("/apps/blog")
	if code != 200 || !strings.Contains(body, "second commit") || !strings.Contains(body, sha(1)[:7]) {
		t.Fatalf("app page: %d\n%s", code, body)
	}
	if code, body, _ := u.get(fmt.Sprintf("/deployments/%d", d1.ID)); code != 200 ||
		!strings.Contains(body, "https://0000000-blog.paas.test") {
		t.Fatalf("deployment page: %d\n%s", code, body)
	}
	if code, _, _ := u.get("/deployments/999999"); code != http.StatusNotFound {
		t.Fatalf("missing deployment: %d", code)
	}

	// Rollback needs a valid CSRF token and a same-origin request.
	rb := "/apps/blog/rollback"
	id := fmt.Sprint(d1.ID)
	if code, _, _ := u.post(rb, url.Values{"deployment_id": {id}}, "self"); code != http.StatusForbidden {
		t.Fatalf("rollback without CSRF: %d, want 403", code)
	}
	csrf := u.csrf("/apps/blog")
	if code, _, _ := u.post(rb, url.Values{"deployment_id": {id}, "csrf": {csrf}}, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin rollback: %d, want 403", code)
	}
	if aliases, _ := u.st.ListAliases(ctx, app.ID); aliases[0].DeploymentID != d2.ID {
		t.Fatal("rejected rollbacks must not move production")
	}
	if code, body, _ := u.post(rb, url.Values{"deployment_id": {id}, "csrf": {csrf}}, "self"); code != http.StatusSeeOther {
		t.Fatalf("rollback: %d\n%s", code, body)
	}
	if aliases, _ := u.st.ListAliases(ctx, app.ID); aliases[0].DeploymentID != d1.ID {
		t.Fatalf("production did not move to deployment %d: %+v", d1.ID, aliases)
	}

	// Env: keys are listed, values never rendered.
	form = url.Values{"key": {"DB_URL"}, "value": {"postgres://s3cret"}, "csrf": {csrf}}
	if code, body, _ := u.post("/apps/blog/env", form, "self"); code >= 400 {
		t.Fatalf("set env: %d\n%s", code, body)
	}
	if _, body, _ := u.get("/apps/blog/settings"); !strings.Contains(body, "DB_URL") || strings.Contains(body, "s3cret") {
		t.Fatal("env key must be listed and its value hidden")
	}

	// Logging out ends the session.
	u.post("/logout", url.Values{"csrf": {csrf}}, "self")
	if code, _, _ := u.get("/"); code != http.StatusSeeOther {
		t.Fatalf("after logout: %d, want redirect to login", code)
	}
}
