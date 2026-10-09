package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/protect"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 24: deployment protection.
//
//	Ayarlar → Koruma (templates/protection.html): mode, scope, password,
//	shareable links and the bypass secret; member+ changes them.
//	/_protect/login (templates/protect_login.html): where Traefik's
//	ForwardAuth sends visitors of a protected host. Team members pass with
//	their platform session; in password mode visitors type the password.
//	Both end in a one-time token on the app host (protect.Guard).

func init() {
	flashes["protection"] = "Koruma ayarları kaydedildi; yönlendirme birkaç saniye içinde güncellenir."
	flashes["share-deleted"] = "Paylaşım bağlantısı iptal edildi."
	flashes["bypass-deleted"] = "Otomasyon anahtarı silindi."
	for en, trk := range map[string]string{
		`mode must be "off", "team" or "password"`:                                          "Koruma Kapalı, Ekip üyeleri ya da Şifre olmalı.",
		`scope must be "previews" or "all"`:                                                 "Kapsam Yalnızca önizlemeler ya da Tüm adresler olmalı.",
		"password must be 8-256 characters":                                                 "Şifre 8-256 karakter olmalı.",
		"password protection needs a password":                                              "Şifre korumasını açmak için bir şifre belirle.",
		"share link name must be at most 100 characters":                                    "Bağlantı adı en fazla 100 karakter olabilir.",
		"expires_in_hours must be between 0 (never) and 8760":                               "Süre 0 (süresiz) ile 8760 saat arasında olmalı.",
		"url must be an http(s) address of this app":                                        "Adres bu projenin http(s) adreslerinden biri olmalı.",
		"share link not found":                                                              "Paylaşım bağlantısı bulunamadı.",
		"too many wrong passwords; try again later":                                         "Çok fazla yanlış şifre denendi; biraz sonra tekrar dene.",
		"traefik CRDs (traefik.io/v1alpha1 IngressRoute, TraefikService) are not available": "Kümede Traefik CRD'leri yok; koruma uygulanamıyor.",
	} {
		normMessages[norm(en)] = trk
	}
	funcs["protectMode"] = label(map[string]string{
		store.ProtectOff: "Kapalı", store.ProtectTeam: "Ekip üyeleri", store.ProtectPassword: "Şifre",
	})
	funcs["protectScope"] = label(map[string]string{
		store.ScopePreviews: "Yalnızca önizlemeler", store.ScopeAll: "Tüm adresler",
	})
}

// protectionTemplates adds the Koruma section to the settings page and the
// login page of protected hosts.
func (s *Server) protectionTemplates() {
	template.Must(s.pages[tabSettings].ParseFS(templateFS, "templates/protection.html"))
	s.pages["protect_login"] = template.Must(template.New("").Funcs(funcs).
		ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/protect_login.html"))
}

func (s *Server) protectionRoutes(mux *http.ServeMux) {
	mux.Handle("POST /apps/{name}/protection", s.authed(s.saveProtection))
	mux.Handle("POST /apps/{name}/protection/share", s.authed(s.createShare))
	mux.Handle("POST /apps/{name}/protection/share/delete", s.authed(s.deleteShare))
	mux.Handle("POST /apps/{name}/protection/bypass", s.authed(s.rotateBypass))
	mux.Handle("POST /apps/{name}/protection/bypass/delete", s.authed(s.deleteBypass))
	// The login page has no session requirement: visitors of password
	// protected hosts have no account.
	mux.HandleFunc("GET "+protect.LoginPath, s.protectLogin)
	mux.HandleFunc("POST "+protect.LoginPath, s.protectPassword)
}

type shareRow struct {
	store.ShareLink
	Expired bool
}

// protectionPanel is the data of the Koruma section.
type protectionPanel struct {
	store.Protection
	PasswordSet bool
	BypassSet   bool
	Links       []shareRow
	// ShareDefault is the address a new link opens: the newest ready
	// preview (production with scope "all").
	ShareDefault string
	// Shown once, right after creation.
	NewShareURL string
	NewBypass   string
	Header      string
}

func (s *Server) protectionPanel(ctx context.Context, app store.App, v appDetail) (*protectionPanel, error) {
	p, err := s.Store.GetProtection(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	links, err := s.Store.ListShareLinks(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	pp := &protectionPanel{Protection: p, PasswordSet: p.PasswordHash != "", BypassSet: p.BypassHash != "",
		ShareDefault: v.ProductionURL, Header: strings.ToLower(protect.BypassHeader)}
	now := time.Now()
	for _, l := range links {
		pp.Links = append(pp.Links, shareRow{ShareLink: l, Expired: l.Expired(now)})
	}
	if p.Scope != store.ScopeAll {
		for _, d := range v.Deployments { // newest first
			if d.Status == store.StatusReady && d.Target == store.EnvPreview {
				pp.ShareDefault = d.URL
				break
			}
		}
	}
	return pp, nil
}

// saveProtection stores the form and pushes it to the router.
func (s *Server) saveProtection(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	p, err := protect.Save(r.Context(), s.Store, app, r.PostFormValue("mode"), r.PostFormValue("scope"), r.PostFormValue("password"))
	if msg, bad := auth.IsInvalid(err); bad {
		s.renderTabOf(w, r, app, http.StatusBadRequest, tabSettings, msg, nil)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if s.Protect != nil {
		s.Protect.Forget(app.Name)
	}
	s.Log.Info("protection updated", "app", app.Name, "mode", p.Mode, "scope", p.Scope,
		"password_changed", r.PostFormValue("password") != "", "by", who(r), "via", "web")
	if s.Router != nil {
		if err := s.Router.SyncApp(r.Context(), app.Name); err != nil {
			s.Log.Error("protection: route sync", "app", app.Name, "err", err)
			s.renderTabOf(w, r, app, http.StatusBadGateway, tabSettings,
				"Koruma kaydedildi ama yönlendirme güncellenemedi; otomatik olarak tekrar denenecek.", nil)
			return
		}
	}
	redirect(w, r, "/apps/"+app.Name+"/settings?ok=protection#protection")
}

func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	fail := func(status int, msg string) { s.renderTabOf(w, r, app, status, tabSettings, msg, nil) }
	hours, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("expires_in_hours")))
	if err != nil {
		hours = 0
	}
	var target *url.URL
	if raw := strings.TrimSpace(r.PostFormValue("url")); raw != "" {
		target, err = protect.AppURL(r.Context(), s.Store, app, s.Domain, raw)
	} else { // the production address, even before the first deployment
		target, err = url.Parse(naming.URL(s.Scheme, naming.ProductionHost(app.Name, s.Domain)))
	}
	if msg, bad := auth.IsInvalid(err); bad {
		fail(http.StatusBadRequest, msg)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	me, _ := auth.From(r.Context())
	l, plain, err := protect.CreateShare(r.Context(), s.Store, app, r.PostFormValue("name"), hours, me.UserID)
	if msg, bad := auth.IsInvalid(err); bad {
		fail(http.StatusBadRequest, msg)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("share link created", "app", app.Name, "link", l.ID, "by", me.Login, "via", "web")
	s.renderTabOf(w, r, app, http.StatusCreated, tabSettings, "", func(v *appDetail) {
		v.Protection.NewShareURL = protect.ShareURL(target, plain)
	})
}

func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err == nil {
		err = s.Store.DeleteShareLink(r.Context(), app.ID, id)
	}
	switch {
	case errors.Is(err, store.ErrNotFound), err != nil && id == 0:
		s.renderTabOf(w, r, app, http.StatusNotFound, tabSettings, "Paylaşım bağlantısı bulunamadı.", nil)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("share link revoked", "app", app.Name, "link", id, "by", who(r), "via", "web")
	redirect(w, r, "/apps/"+app.Name+"/settings?ok=share-deleted#protection")
}

func (s *Server) rotateBypass(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	secret, err := protect.RotateBypass(r.Context(), s.Store, app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if s.Protect != nil {
		s.Protect.Forget(app.Name)
	}
	s.Log.Info("protection bypass rotated", "app", app.Name, "by", who(r), "via", "web")
	s.renderTabOf(w, r, app, http.StatusCreated, tabSettings, "", func(v *appDetail) {
		v.Protection.NewBypass = secret
	})
}

func (s *Server) deleteBypass(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	if err := protect.RemoveBypass(r.Context(), s.Store, app); err != nil {
		s.internalError(w, r, err)
		return
	}
	if s.Protect != nil {
		s.Protect.Forget(app.Name)
	}
	s.Log.Info("protection bypass removed", "app", app.Name, "by", who(r), "via", "web")
	redirect(w, r, "/apps/"+app.Name+"/settings?ok=bypass-deleted#protection")
}

// ---- the login page of protected hosts ----

// loginTarget validates ?app= and ?next= (or the form fields); false when
// it already answered.
func (s *Server) loginTarget(w http.ResponseWriter, r *http.Request, appName, next string) (store.App, store.Protection, *url.URL, bool) {
	if s.Protect == nil {
		s.errorPage(w, r, http.StatusNotFound, "Sayfa bulunamadı.")
		return store.App{}, store.Protection{}, nil, false
	}
	app, p, target, err := s.Protect.LoginTarget(r.Context(), appName, next)
	_, bad := auth.IsInvalid(err)
	if errors.Is(err, store.ErrNotFound) || bad {
		s.errorPage(w, r, http.StatusBadRequest, "Geçersiz giriş bağlantısı. Korumalı adresi yeniden aç.")
		return app, p, nil, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return app, p, nil, false
	}
	return app, p, target, true
}

func (s *Server) protectLogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	app, p, target, ok := s.loginTarget(w, r, q.Get("app"), q.Get("next"))
	if !ok {
		return
	}
	if !p.Enabled() {
		http.Redirect(w, r, target.String(), http.StatusSeeOther)
		return
	}
	me, err := s.Auth.Session(r)
	if err != nil && !errors.Is(err, auth.ErrUnauthenticated) {
		s.internalError(w, r, err)
		return
	}
	signedIn := err == nil
	if signedIn {
		role, err := s.Auth.TeamRole(r.Context(), me, app.TeamID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if role != "" {
			s.Log.Info("protect: member login", "app", app.Name, "user", me.Login, "host", target.Host)
			http.Redirect(w, r, s.Protect.TokenURL(app, p, target, protect.KindTeam, strconv.FormatInt(me.UserID, 10)), http.StatusSeeOther)
			return
		}
	}
	signIn := "/login?next=" + url.QueryEscape(r.URL.RequestURI())
	if p.Mode == store.ProtectTeam {
		if !signedIn {
			http.Redirect(w, r, signIn, http.StatusSeeOther)
			return
		}
		s.errorPage(w, r, http.StatusForbidden,
			"Bu deploy korumalı: yalnızca projenin ekip üyeleri açabilir. @"+me.Login+" hesabı bu ekipte değil.")
		return
	}
	s.renderProtectLogin(w, r, http.StatusOK, app, target, signIn, signedIn, "")
}

func (s *Server) renderProtectLogin(w http.ResponseWriter, r *http.Request, status int, app store.App, target *url.URL, signIn string, signedIn bool, errMsg string) {
	s.render(w, r, status, "protect_login", "Korumalı deploy", map[string]any{
		"App": app.Name, "Host": target.Host, "Next": target.String(), "SignIn": signIn, "SignedIn": signedIn,
	}, errMsg)
}

// protectPassword checks the password form.
func (s *Server) protectPassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if !auth.SameOrigin(r) {
		http.Error(w, "başka siteden gelen istek reddedildi", http.StatusForbidden)
		return
	}
	app, p, target, ok := s.loginTarget(w, r, r.PostFormValue("app"), r.PostFormValue("next"))
	if !ok {
		return
	}
	retry := protect.LoginPath + "?" + url.Values{"app": {app.Name}, "next": {target.String()}}.Encode()
	if p.Mode != store.ProtectPassword {
		http.Redirect(w, r, retry, http.StatusSeeOther)
		return
	}
	signIn := "/login?next=" + url.QueryEscape(retry)
	_, err := s.Auth.Session(r)
	signedIn := err == nil
	ok, err = s.Protect.CheckPassword(p, protect.ClientKey(r), r.PostFormValue("password"))
	if errors.Is(err, protect.ErrTooManyAttempts) {
		s.Log.Warn("protect: password attempts limited", "app", app.Name, "client", protect.ClientKey(r))
		s.renderProtectLogin(w, r, http.StatusTooManyRequests, app, target, signIn, signedIn, err.Error())
		return
	}
	if !ok {
		s.Log.Warn("protect: wrong password", "app", app.Name, "client", protect.ClientKey(r))
		s.renderProtectLogin(w, r, http.StatusUnauthorized, app, target, signIn, signedIn, "Şifre yanlış.")
		return
	}
	s.Log.Info("protect: password login", "app", app.Name, "host", target.Host)
	// A page instead of a redirect: the CSP's form-action 'self' would
	// block a redirect to the app host after a form post.
	s.render(w, r, http.StatusOK, "redirect", "Korumalı deploy", map[string]string{
		"Next": s.Protect.TokenURL(app, p, target, protect.KindPassword, ""), "Message": "Şifre doğru.",
	}, "")
}
