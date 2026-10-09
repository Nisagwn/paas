package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/protect"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/web"
)

// Faz 24: the Koruma section and the login page of protected hosts.

func setupProtect(t *testing.T) (*f20, *protect.Guard) {
	protect.Iterations = 1000
	var g *protect.Guard
	f := setupF20(t, func(s *web.Server) {
		g = &protect.Guard{Store: s.Store, Sessions: s.Sessions, Auth: &auth.Authenticator{Store: s.Store, Sessions: s.Sessions},
			Domain: "paas.test", PublicURL: "https://paas.test", CacheTTL: -1,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		s.Protect = g
	})
	ctx := context.Background()
	d, _, _ := f.st.EnqueueDeployment(ctx, f.app.ID, sha(1), "feature", "x")
	f.st.MarkReady(ctx, d, []store.AliasSpec{{Hostname: "feature-blog.paas.test", Kind: store.AliasPreview, Branch: "feature"}})
	return f, g
}

func TestProtectionSettingsForm(t *testing.T) {
	f, _ := setupProtect(t)
	const tab, path = "/apps/blog/settings", "/apps/blog/protection"
	_, body, _ := f.owner.get(tab)
	mustContain(t, "settings", body, `id="protection"`, "Koruma", "Yalnızca ekip üyeleri", "Paylaşım bağlantıları", "Otomasyon anahtarı")
	_, body, _ = f.viewer.get(tab)
	mustContain(t, "viewer settings", body, "Koruma", "yönetmek için ekipte üye rolü gerekir")
	mustNotContain(t, "viewer settings", body, `action="/apps/blog/protection/share"`)

	f.formChecks(path, url.Values{"mode": {"team"}})
	csrf := f.owner.csrf(tab)
	if code, body, _ := f.owner.post(path, url.Values{"csrf": {csrf}, "mode": {"password"}}, "self"); code != http.StatusBadRequest ||
		!strings.Contains(body, "Şifre korumasını açmak için bir şifre belirle.") {
		t.Fatalf("password without password: %d", code)
	}
	if code, body, _ := f.owner.post(path, url.Values{"csrf": {csrf}, "mode": {"password"}, "password": {"short"}}, "self"); code != http.StatusBadRequest ||
		!strings.Contains(body, "Şifre 8-256 karakter olmalı.") {
		t.Fatalf("short password: %d", code)
	}
	code, _, h := f.owner.post(path, url.Values{"csrf": {csrf}, "mode": {"password"}, "scope": {"all"}, "password": {"open sesame!"}}, "self")
	if code != http.StatusSeeOther || h.Get("Location") != tab+"?ok=protection#protection" {
		t.Fatalf("save: %d %s", code, h.Get("Location"))
	}
	p, _ := f.st.GetProtection(context.Background(), f.app.ID)
	if p.Mode != store.ProtectPassword || p.Scope != store.ScopeAll || p.PasswordHash == "" {
		t.Fatalf("stored: %+v", p)
	}
	_, body, _ = f.owner.get(tab + "?ok=protection")
	mustContain(t, "saved", body, "Koruma ayarları kaydedildi", "ayarlı; boş bırakırsan değişmez")

	// Share link: shown once, listed by prefix.
	f.formChecks(path+"/share", url.Values{"name": {"x"}})
	code, body, _ = f.owner.post(path+"/share", url.Values{"csrf": {csrf}, "name": {"müşteri"}, "expires_in_hours": {"24"},
		"url": {"http://feature-blog.paas.test/docs"}}, "self")
	m := regexp.MustCompile(`http://feature-blog\.paas\.test/docs\?paas_share=(paas_share_[A-Za-z0-9_-]+)`).FindStringSubmatch(body)
	if code != http.StatusCreated || m == nil {
		t.Fatalf("share: %d", code)
	}
	if code, body, _ := f.owner.post(path+"/share", url.Values{"csrf": {csrf}, "url": {"https://evil.example/"}}, "self"); code != http.StatusBadRequest ||
		!strings.Contains(body, "Adres bu projenin http(s) adreslerinden biri olmalı.") {
		t.Fatalf("foreign share url: %d", code)
	}
	_, body, _ = f.owner.get(tab)
	mustContain(t, "share listing", body, "müşteri", "paas_share_")
	mustNotContain(t, "share listing", body, m[1])
	links, _ := f.st.ListShareLinks(context.Background(), f.app.ID)
	del := url.Values{"csrf": {csrf}, "id": {itoa(links[0].ID)}}
	if code, _, _ := f.owner.post(path+"/share/delete", del, "self"); code != http.StatusSeeOther {
		t.Fatalf("delete share: %d", code)
	}

	// Bypass secret: shown once.
	f.formChecks(path+"/bypass", url.Values{})
	code, body, _ = f.owner.post(path+"/bypass", url.Values{"csrf": {csrf}}, "self")
	if code != http.StatusCreated || !strings.Contains(body, "paas_bypass_") || !strings.Contains(body, "x-paas-protection-bypass") {
		t.Fatalf("bypass: %d", code)
	}
	if code, _, _ := f.owner.post(path+"/bypass/delete", url.Values{"csrf": {csrf}}, "self"); code != http.StatusSeeOther {
		t.Fatalf("delete bypass: %d", code)
	}
}

func TestProtectLoginPage(t *testing.T) {
	f, g := setupProtect(t)
	ctx := context.Background()
	next := "http://feature-blog.paas.test/docs"
	login := "/_protect/login?" + url.Values{"app": {"blog"}, "next": {next}}.Encode()

	// Off: straight back.
	if code, _, h := f.ui.get(login); code != http.StatusSeeOther || h.Get("Location") != next {
		t.Fatalf("off: %d %s", code, h.Get("Location"))
	}
	protect.Save(ctx, f.st, f.app, store.ProtectTeam, "", "")
	// Foreign next: refused.
	bad := "/_protect/login?" + url.Values{"app": {"blog"}, "next": {"https://evil.example/"}}.Encode()
	if code, _, _ := f.owner.get(bad); code != http.StatusBadRequest {
		t.Fatalf("foreign next: %d", code)
	}
	// Team mode, not signed in: platform login first.
	if code, _, h := f.ui.get(login); code != http.StatusSeeOther || !strings.HasPrefix(h.Get("Location"), "/login?next=") {
		t.Fatalf("anonymous: %d %s", code, h.Get("Location"))
	}
	// A member (viewer is enough) gets a token on the app host.
	code, _, h := f.viewer.get(login)
	loc, _ := url.Parse(h.Get("Location"))
	if code != http.StatusSeeOther || loc == nil || loc.Host != "feature-blog.paas.test" || loc.Path != protect.AuthPath || loc.Query().Get("token") == "" {
		t.Fatalf("member: %d %s", code, h.Get("Location"))
	}
	// The token sets the cookie through the verify endpoint.
	r, _ := http.NewRequest("GET", protect.VerifyPath+"?app=blog", nil)
	r.Header.Set("X-Forwarded-Host", "feature-blog.paas.test")
	r.Header.Set("X-Forwarded-Uri", loc.RequestURI())
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusFound || w.Header().Get("Location") != next || !strings.Contains(w.Header().Get("Set-Cookie"), protect.CookieName+"=") {
		t.Fatalf("redeem: %d %v", w.Code, w.Header())
	}
	// A signed-in outsider is refused.
	ctxUser, _ := f.st.UpsertUser(ctx, 9, "mallory", "", "")
	if code, body, _ := f.ui.as(ctxUser.ID).get(login); code != http.StatusForbidden || !strings.Contains(body, "@mallory") {
		t.Fatalf("outsider: %d", code)
	}

	// Password mode: the form, a wrong and a right password.
	protect.Save(ctx, f.st, f.app, store.ProtectPassword, "", "open sesame!")
	code, body, _ := f.ui.get(login)
	mustContain(t, "password page", body, "Korumalı deploy", `name="password"`, "feature-blog.paas.test")
	if code != http.StatusOK {
		t.Fatalf("password page: %d", code)
	}
	form := url.Values{"app": {"blog"}, "next": {next}, "password": {"wrong one"}}
	if code, body, _ := f.ui.post("/_protect/login", form, "self"); code != http.StatusUnauthorized || !strings.Contains(body, "Şifre yanlış.") {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _, _ := f.ui.post("/_protect/login", form, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", code)
	}
	form.Set("password", "open sesame!")
	code, body, _ = f.ui.post("/_protect/login", form, "self")
	if code != http.StatusOK || !strings.Contains(body, "http://feature-blog.paas.test/_paas/auth?token=") {
		t.Fatalf("right password: %d %s", code, body)
	}
	// Members skip the password.
	if code, _, h := f.owner.get(login); code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), protect.AuthPath) {
		t.Fatalf("member in password mode: %d %s", code, h.Get("Location"))
	}
}
