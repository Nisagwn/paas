package protect

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

const domain = "paas.test"

type fixture struct {
	t     *testing.T
	st    *store.Store
	g     *Guard
	app   store.App
	alice store.User // member of the app's team
	eve   store.User // in no team
	host  string     // a preview alias of the app
}

func setup(t *testing.T) *fixture {
	t.Helper()
	Iterations = 1000
	st := testdb.Open(t)
	ctx := context.Background()
	alice, _ := st.UpsertUser(ctx, 1, "alice", "", "")
	eve, _ := st.UpsertUser(ctx, 2, "eve", "", "")
	team, err := st.CreateTeam(ctx, "web", "Web", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateAppInTeam(ctx, team.ID, "blog", "o/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("a", 40), "feature", "x")
	if err := st.MarkReady(ctx, d, []store.AliasSpec{{Hostname: "feature-blog." + domain, Kind: store.AliasPreview, Branch: "feature"}}); err != nil {
		t.Fatal(err)
	}
	sessions := auth.New("admin-token")
	g := &Guard{
		Store: st, Sessions: sessions, Auth: &auth.Authenticator{Store: st, Sessions: sessions},
		Domain: domain, PublicURL: "https://" + domain, CacheTTL: -1,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return &fixture{t: t, st: st, g: g, app: app, alice: alice, eve: eve, host: "feature-blog." + domain}
}

func (f *fixture) protect(mode, scope, password string) store.Protection {
	f.t.Helper()
	p, err := Save(context.Background(), f.st, f.app, mode, scope, password)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// verify plays Traefik's ForwardAuth subrequest for a request to host+uri.
func (f *fixture) verify(host, uri string, mod func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest("GET", VerifyPath+"?app=blog", nil)
	r.Header.Set("X-Forwarded-Host", host)
	r.Header.Set("X-Forwarded-Uri", uri)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Method", "GET")
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	f.g.ServeHTTP(w, r)
	return w
}

func withCookie(v string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: v}) }
}

func cookieOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie: %v", CookieName, w.Header())
	return nil
}

func TestPasswordHash(t *testing.T) {
	Iterations = 1000
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$1000$") || !CheckPassword(h, "correct horse") || CheckPassword(h, "wrong horse") {
		t.Fatalf("hash %q", h)
	}
	h2, _ := HashPassword("correct horse")
	if h == h2 {
		t.Fatal("salt is not random")
	}
	for _, bad := range []string{"", "x$1$a$b", "pbkdf2-sha256$0$AA$AA", "pbkdf2-sha256$1000$!!$AA"} {
		if CheckPassword(bad, "") {
			t.Errorf("malformed hash %q matched", bad)
		}
	}
}

func TestVerifyOffAndUnknown(t *testing.T) {
	f := setup(t)
	if w := f.verify(f.host, "/", nil); w.Code != 200 {
		t.Fatalf("off: %d", w.Code)
	}
	r := httptest.NewRequest("GET", VerifyPath+"?app=nope", nil)
	w := httptest.NewRecorder()
	f.g.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("unknown app: %d", w.Code)
	}
}

func TestVerifyCookie(t *testing.T) {
	f := setup(t)
	p := f.protect(store.ProtectTeam, store.ScopePreviews, "")

	// No cookie: GET goes to the login page with the full URL; POST gets 401.
	w := f.verify(f.host, "/docs?x=1", nil)
	loc := w.Header().Get("Location")
	if w.Code != http.StatusFound || !strings.HasPrefix(loc, "https://paas.test/_protect/login?") ||
		!strings.Contains(loc, url.QueryEscape("https://"+f.host+"/docs?x=1")) {
		t.Fatalf("no cookie: %d %q", w.Code, loc)
	}
	if w := f.verify(f.host, "/", func(r *http.Request) { r.Header.Set("X-Forwarded-Method", "POST") }); w.Code != 401 {
		t.Fatalf("POST without cookie: %d", w.Code)
	}

	uid := strconv.FormatInt(f.alice.ID, 10)
	good := f.g.sealCookieValue("blog", grant{Host: f.host, Version: p.Version, Kind: KindTeam, Subject: uid}, time.Hour)
	if w := f.verify(f.host, "/", withCookie(good)); w.Code != 200 {
		t.Fatalf("valid cookie: %d %s", w.Code, w.Body)
	}
	for name, v := range map[string]string{
		"expired":       f.g.sealCookieValue("blog", grant{Host: f.host, Version: p.Version, Kind: KindTeam, Subject: uid}, -time.Second),
		"tampered":      good[:len(good)-2] + "xx",
		"other app":     f.g.sealCookieValue("infra", grant{Host: f.host, Version: p.Version, Kind: KindTeam, Subject: uid}, time.Hour),
		"old version":   f.g.sealCookieValue("blog", grant{Host: f.host, Version: p.Version - 1, Kind: KindTeam, Subject: uid}, time.Hour),
		"not a member":  f.g.sealCookieValue("blog", grant{Host: f.host, Version: p.Version, Kind: KindTeam, Subject: strconv.FormatInt(f.eve.ID, 10)}, time.Hour),
		"password kind": f.g.sealCookieValue("blog", grant{Host: f.host, Version: p.Version, Kind: KindPassword}, time.Hour),
		"garbage":       "nonsense",
	} {
		if w := f.verify(f.host, "/", withCookie(v)); w.Code != http.StatusFound {
			t.Errorf("%s cookie: %d", name, w.Code)
		}
	}
	// A cookie of one host is useless on another.
	if w := f.verify("a3f9c1d-blog."+domain, "/", withCookie(good)); w.Code != http.StatusFound {
		t.Errorf("wrong host: %d", w.Code)
	}
	// Changing the mode signs everyone out.
	f.protect(store.ProtectPassword, "", "hunter2hunter2")
	f.protect(store.ProtectTeam, "", "")
	if w := f.verify(f.host, "/", withCookie(good)); w.Code != http.StatusFound {
		t.Errorf("cookie after mode change: %d", w.Code)
	}
}

// TestTeamFlow: login page token → /_paas/auth on the app host → cookie →
// access; the token works once and only on its host.
func TestTeamFlow(t *testing.T) {
	f := setup(t)
	p := f.protect(store.ProtectTeam, store.ScopePreviews, "")
	app, p2, target, err := f.g.LoginTarget(context.Background(), "blog", "https://"+f.host+"/docs?x=1")
	if err != nil || p2.Version != p.Version {
		t.Fatal(err)
	}
	tokURL, _ := url.Parse(f.g.TokenURL(app, p2, target, KindTeam, strconv.FormatInt(f.alice.ID, 10)))
	if tokURL.Host != f.host || tokURL.Path != AuthPath {
		t.Fatalf("token url %v", tokURL)
	}
	// Redeemed on another host: refused.
	if w := f.verify("a3f9c1d-blog."+domain, tokURL.RequestURI(), nil); w.Code != 403 {
		t.Fatalf("other host: %d", w.Code)
	}
	w := f.verify(f.host, tokURL.RequestURI(), nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://"+f.host+"/docs?x=1" {
		t.Fatalf("redeem: %d %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	c := cookieOf(t, w)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Domain != "" || c.Path != "/" {
		t.Fatalf("cookie attributes: %+v", c)
	}
	if w := f.verify(f.host, "/docs", withCookie(c.Value)); w.Code != 200 {
		t.Fatalf("with cookie: %d", w.Code)
	}
	// One time only.
	if w := f.verify(f.host, tokURL.RequestURI(), nil); w.Code != 403 {
		t.Fatalf("second redeem: %d", w.Code)
	}
	// Removing alice from the team ends her access.
	bob, _ := f.st.UpsertUser(context.Background(), 3, "bob", "", "")
	f.st.SetMember(context.Background(), f.app.TeamID, bob.ID, store.RoleOwner)
	if err := f.st.RemoveMember(context.Background(), f.app.TeamID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if w := f.verify(f.host, "/docs", withCookie(c.Value)); w.Code != http.StatusFound {
		t.Fatalf("after removal: %d", w.Code)
	}
	// Plain HTTP (k3d): the cookie is not Secure.
	tokURL, _ = url.Parse(f.g.TokenURL(app, p2, target, KindTeam, strconv.FormatInt(bob.ID, 10)))
	w = f.verify(f.host, tokURL.RequestURI(), func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") })
	if c := cookieOf(t, w); c.Secure || !strings.HasPrefix(w.Header().Get("Location"), "http://") {
		t.Fatalf("http: secure=%v location=%q", c.Secure, w.Header().Get("Location"))
	}
}

func TestLoginTargetMustBeAppHost(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.st.AddDomain(ctx, f.app.ID, "www.example.com", "tok")
	for next, ok := range map[string]bool{
		"https://" + f.host + "/":             true,
		"http://feature-blog.paas.test:8080/": true,
		"https://aaaaaaa-blog.paas.test/":     true,  // deployment URL
		"https://aaaaaaa-1-blog.paas.test/":   false, // no such generation
		"https://bbbbbbb-blog.paas.test/":     false,
		"https://www.example.com/a":           true,
		"https://evil.example/":               false,
		"https://feature-my-blog.paas.test/":  false,
		"javascript:alert(1)":                 false,
		"/relative":                           false,
	} {
		_, _, _, err := f.g.LoginTarget(ctx, "blog", next)
		if (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", next, err, ok)
		}
	}
}

func TestPasswordFlowAndLimit(t *testing.T) {
	f := setup(t)
	if _, err := Save(context.Background(), f.st, f.app, store.ProtectPassword, "", ""); err == nil {
		t.Fatal("password mode without a password")
	}
	if _, err := Save(context.Background(), f.st, f.app, store.ProtectPassword, "", "short"); err == nil {
		t.Fatal("short password accepted")
	}
	p := f.protect(store.ProtectPassword, store.ScopeAll, "open sesame!")
	if strings.Contains(p.PasswordHash, "open sesame") {
		t.Fatal("password stored in clear")
	}
	f.g.MaxFailures = 3
	if ok, _ := f.g.CheckPassword(p, "1.2.3.4", "open sesame!"); !ok {
		t.Fatal("right password refused")
	}
	for i := 0; i < 3; i++ {
		if ok, err := f.g.CheckPassword(p, "1.2.3.4", "nope"); ok || err != nil {
			t.Fatalf("wrong password %d: %v %v", i, ok, err)
		}
	}
	if _, err := f.g.CheckPassword(p, "1.2.3.4", "open sesame!"); err != ErrTooManyAttempts {
		t.Fatalf("limit: %v", err)
	}
	if ok, _ := f.g.CheckPassword(p, "5.6.7.8", "open sesame!"); !ok {
		t.Fatal("other client limited")
	}
	app, p, target, _ := f.g.LoginTarget(context.Background(), "blog", "https://"+f.host+"/")
	tok, _ := url.Parse(f.g.TokenURL(app, p, target, KindPassword, ""))
	c := cookieOf(t, f.verify(f.host, tok.RequestURI(), nil))
	if w := f.verify(f.host, "/", withCookie(c.Value)); w.Code != 200 {
		t.Fatalf("password cookie: %d", w.Code)
	}
	// A new password signs password visitors out.
	f.protect("", "", "another secret")
	if w := f.verify(f.host, "/", withCookie(c.Value)); w.Code != http.StatusFound {
		t.Fatalf("after password change: %d", w.Code)
	}
}

func TestShareLinksAndBypass(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.protect(store.ProtectTeam, store.ScopePreviews, "")
	l, plain, err := CreateShare(ctx, f.st, f.app, "review", 0, f.alice.ID)
	if err != nil || !strings.HasPrefix(plain, SharePrefix) || l.CreatedBy != "alice" {
		t.Fatalf("create: %v %q %+v", err, plain, l)
	}
	w := f.verify(f.host, "/page?a=1&"+ShareParam+"="+plain, nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://"+f.host+"/page?a=1" {
		t.Fatalf("share: %d %q", w.Code, w.Header().Get("Location"))
	}
	c := cookieOf(t, w)
	if w := f.verify(f.host, "/page", withCookie(c.Value)); w.Code != 200 {
		t.Fatalf("share cookie: %d", w.Code)
	}
	if w := f.verify(f.host, "/?"+ShareParam+"=paas_share_bogus", nil); w.Code != 403 {
		t.Fatalf("unknown token: %d", w.Code)
	}
	// Revoking the link ends access through its cookie.
	if err := f.st.DeleteShareLink(ctx, f.app.ID, l.ID); err != nil {
		t.Fatal(err)
	}
	if w := f.verify(f.host, "/page", withCookie(c.Value)); w.Code != http.StatusFound {
		t.Fatalf("revoked: %d", w.Code)
	}
	if _, _, err := CreateShare(ctx, f.st, f.app, "", -1, 0); err == nil {
		t.Fatal("negative expiry")
	}
	// Expired links are refused.
	exp, plain2, _ := CreateShare(ctx, f.st, f.app, "old", 1, 0)
	past := time.Now().Add(-time.Hour)
	if _, err := f.st.UseShareLink(ctx, f.app.ID, HashSecret(plain2)); err != nil {
		t.Fatal(err)
	}
	if err := setShareExpiry(f.st, exp.ID, past); err != nil {
		t.Fatal(err)
	}
	if w := f.verify(f.host, "/?"+ShareParam+"="+plain2, nil); w.Code != 403 {
		t.Fatalf("expired: %d", w.Code)
	}

	// Bypass header.
	secret, err := RotateBypass(ctx, f.st, f.app)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.verify(f.host, "/", func(r *http.Request) { r.Header.Set(BypassHeader, secret) }); w.Code != 200 {
		t.Fatalf("bypass: %d", w.Code)
	}
	if w := f.verify(f.host, "/", func(r *http.Request) { r.Header.Set(BypassHeader, secret+"x") }); w.Code != http.StatusFound {
		t.Fatalf("wrong bypass: %d", w.Code)
	}
	newSecret, _ := RotateBypass(ctx, f.st, f.app)
	if w := f.verify(f.host, "/", func(r *http.Request) { r.Header.Set(BypassHeader, secret) }); w.Code != http.StatusFound {
		t.Fatalf("rotated-out bypass: %d", w.Code)
	}
	RemoveBypass(ctx, f.st, f.app)
	if w := f.verify(f.host, "/", func(r *http.Request) { r.Header.Set(BypassHeader, newSecret) }); w.Code != http.StatusFound {
		t.Fatalf("removed bypass: %d", w.Code)
	}
}

// setShareExpiry moves a link's expiry into the past (raw SQL: the store
// never shortens a link).
func setShareExpiry(_ *store.Store, id int64, t time.Time) error {
	db, err := sql.Open("postgres", os.Getenv("PAAS_TEST_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE protection_share_links SET expires_at = $1 WHERE id = $2`, t, id)
	return err
}
