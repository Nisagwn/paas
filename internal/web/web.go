// Package web serves the browser UI: server-rendered html/template pages,
// htmx for partial refreshes, and EventSource for live logs. There is no
// build step; templates are embedded in the binary.
//
// Authentication is GitHub OAuth (Faz 13; oauth.go) or, in dev mode without
// an OAuth App, the API token entered once on /login. Both end in a signed
// session cookie (internal/auth). Every page checks the caller's role on the
// app's team (users.go). Every state-changing form is a POST that must carry
// the session's CSRF token and, when the browser sends one, a same-host
// Origin.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	Store *store.Store
	// Router is nil when nothing is routed (dry run).
	Router   api.Router
	Sessions *auth.Sessions
	Domain   string
	// Scheme of app links; empty means "https".
	Scheme string
	// RuntimeLogs shows the runtime log panel (Kubernetes deployer only).
	RuntimeLogs bool
	Log         *slog.Logger
	// Domains checks custom domains on demand (Faz 12, optional).
	Domains api.DomainChecker
	// Faz 13. Auth nil builds one from Store and Sessions (token login only).
	Auth *auth.Authenticator
	// GitHub enables "Sign in with GitHub"; nil is dev mode (token login).
	GitHub *auth.GitHubLogin

	// Faz 15 (github.go). GitHubAppSlug is the App's URL name
	// (github.com/apps/<slug>); empty hides the install button. GitHubApp
	// reads repositories for imports; nil creates apps without a deployment.
	GitHubAppSlug string
	GitHubApp     api.RepoInspector

	// Faz 16–19 screens (analytics.go). Usage reads live CPU/memory; nil (dry run)
	// shows "no usage data". Now is the analytics clock (tests); nil is
	// time.Now.
	Usage api.UsageSource
	Now   func() time.Time

	pages map[string]*template.Template
}

var funcs = template.FuncMap{
	"short": naming.ShortSHA,
	"firstLine": func(s string) string {
		s, _, _ = strings.Cut(s, "\n")
		return s
	},
	"ago":    ago,
	"time":   func(t time.Time) string { return t.UTC().Format("02.01.2006 15:04:05 UTC") },
	"tr":     tr,
	"status": statusLabel,
	"role":   roleLabel,
	"kind":   kindLabel,
	// Faz 16–19 screens.
	"target": targetLabel,
	"origin": originLabel,
	"health": healthLabel,
	"envtarget": func(t string) string {
		if t == store.EnvAll {
			return "Tümü"
		}
		return targetLabel(t)
	},
	"pct":   pct,
	"ms":    millis,
	"bytes": byteSize,
	"cpu":   cpuLabel,
	"num":   number,
	"active": func(status string) bool {
		return status == store.StatusQueued || status == store.StatusBuilding || status == store.StatusDeploying
	},
	"duration": func(d store.Deployment) string {
		if d.StartedAt == nil || d.FinishedAt == nil {
			return ""
		}
		return shortDuration(d.FinishedAt.Sub(*d.StartedAt))
	},
}

func (s *Server) Handler() http.Handler {
	s.pages = map[string]*template.Template{}
	if s.Auth == nil {
		s.Auth = &auth.Authenticator{Store: s.Store, Sessions: s.Sessions, Log: s.Log}
	}
	for _, p := range []string{"login", "apps", "app", "deployment", "error", "teams", "team", "tokens", "redirect", "import",
		"deploys", "analytics", "settings"} {
		s.pages[p] = template.Must(template.New("").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+p+".html"))
	}
	partials := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/partials.html"))
	s.pages["deployments"] = partials.Lookup("deployments")
	s.pages["env"] = partials.Lookup("env")
	s.pages["domains"] = partials.Lookup("domains")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.Handle("POST /logout", s.authed(s.logout))
	mux.Handle("GET /{$}", s.authed(s.appsPage))
	mux.Handle("POST /apps", s.authed(s.createApp))
	mux.Handle("GET /apps/{name}", s.authed(s.appPage))
	// Faz 16–19 screens: the app's tabs (app.go, controls.go, analytics.go). The
	// deployments tab is also the htmx fragment polled while one runs.
	mux.Handle("GET /apps/{name}/deployments", s.authed(s.deploymentsPage))
	mux.Handle("GET /apps/{name}/analytics", s.authed(s.analyticsPage))
	mux.Handle("GET /apps/{name}/settings", s.authed(s.settingsPage))
	mux.Handle("POST /apps/{name}/settings", s.authed(s.saveSettings))
	mux.Handle("POST /apps/{name}/promote", s.authed(s.promote))
	mux.Handle("POST /apps/{name}/redeploy", s.authed(s.redeploy))
	mux.Handle("POST /apps/{name}/cancel", s.authed(s.cancel))
	mux.Handle("POST /apps/{name}/hooks", s.authed(s.createHook))
	mux.Handle("POST /apps/{name}/hooks/delete", s.authed(s.deleteHook))
	mux.Handle("POST /apps/{name}/rollback", s.authed(s.rollback))
	mux.Handle("POST /apps/{name}/env", s.authed(s.setEnv))
	mux.Handle("POST /apps/{name}/env/delete", s.authed(s.deleteEnv))
	mux.Handle("POST /apps/{name}/domains", s.authed(s.addDomain))
	mux.Handle("POST /apps/{name}/domains/verify", s.authed(s.verifyDomain))
	mux.Handle("POST /apps/{name}/domains/delete", s.authed(s.deleteDomain))
	mux.Handle("GET /deployments/{id}", s.authed(s.deploymentPage))
	// Faz 13: GitHub login, teams and personal tokens (oauth.go, users.go).
	mux.HandleFunc("GET /auth/github", s.githubStart)
	mux.HandleFunc("GET /auth/github/callback", s.githubCallback)
	mux.Handle("GET /teams", s.authed(s.teamsPage))
	mux.Handle("POST /teams", s.authed(s.createTeam))
	mux.Handle("GET /teams/{slug}", s.authed(s.teamPage))
	mux.Handle("POST /teams/{slug}/members", s.authed(s.setMember))
	mux.Handle("POST /teams/{slug}/members/remove", s.authed(s.removeMember))
	mux.Handle("GET /tokens", s.authed(s.tokensPage))
	mux.Handle("POST /tokens", s.authed(s.createToken))
	mux.Handle("POST /tokens/revoke", s.authed(s.revokeToken))
	// Faz 15: GitHub App install, setup URL and import (github.go). The
	// setup URL is a redirect from github.com and carries no session.
	mux.Handle("GET /github/install", s.authed(s.githubInstall))
	mux.HandleFunc("GET /github/setup", s.githubSetup)
	mux.Handle("GET /import", s.authed(s.importPage))
	mux.Handle("POST /import", s.authed(s.importApp))
	mux.Handle("/", s.authed(func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, http.StatusNotFound, "Sayfa bulunamadı.")
	}))
	return securityHeaders(mux)
}

// ---- middleware ----

type ctxKey int

const nonceKey ctxKey = 0

// securityHeaders sets a strict CSP with a per-request nonce for the inline
// scripts; htmx is the only external script.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 16)
		rand.Read(b)
		nonce := base64.StdEncoding.EncodeToString(b)
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'nonce-"+nonce+"' https://unpkg.com; "+
			"style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; "+
			"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nonceKey, nonce)))
	})
}

// authed accepts a bearer token (legacy admin or personal; scripts) or a
// session cookie (browsers) and stores the caller in the request context.
// Cookie-authenticated POSTs must pass the CSRF check; a bearer header
// cannot be attached by another site, so it needs none.
func (s *Server) authed(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			p, err := s.Auth.Bearer(r.Context(), token)
			if err != nil {
				http.Error(w, "geçersiz bearer token", http.StatusUnauthorized)
				return
			}
			h(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
			return
		}
		p, err := s.Auth.Session(r)
		if err != nil && !errors.Is(err, auth.ErrUnauthenticated) {
			s.internalError(w, r, err)
			return
		}
		if err != nil {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "giriş yapmalısın", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			if !s.Sessions.CheckCSRF(r) {
				http.Error(w, "geçersiz CSRF token'ı ya da başka siteden gelen istek; sayfayı yenileyip tekrar dene", http.StatusForbidden)
				return
			}
		}
		h(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// ---- rendering ----

// page is the data every template receives.
type page struct {
	Title    string
	CSRF     string
	Nonce    string
	LoggedIn bool
	Flash    string
	Error    string
	Domain   string
	EnvError string // only set on htmx env fragments
	// DomainError is only set on htmx domain fragments.
	DomainError string
	Data        any
	// Me is the signed-in caller (Faz 13).
	Me auth.Principal
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name, title string, data any, errMsg string) {
	p := page{
		Title: tr(title), CSRF: s.Sessions.CSRFToken(r), Domain: s.Domain, Data: data, Error: tr(errMsg),
		Flash: flashes[r.URL.Query().Get("ok")],
	}
	p.Nonce, _ = r.Context().Value(nonceKey).(string)
	p.Me, p.LoggedIn = auth.From(r.Context())
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		s.Log.Error("render", "page", name, "err", err)
		http.Error(w, "sunucu hatası", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// partial renders a fragment for htmx.
func (s *Server) partial(w http.ResponseWriter, r *http.Request, status int, name string, data any, envErr string) {
	var buf bytes.Buffer
	if err := s.pages[name].Execute(&buf, page{Data: data, CSRF: s.Sessions.CSRFToken(r), EnvError: tr(envErr)}); err != nil {
		s.Log.Error("render", "partial", name, "err", err)
		http.Error(w, "sunucu hatası", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// redirect sends a browser to path after a successful POST; htmx requests
// get HX-Redirect, which performs a full navigation.
func redirect(w http.ResponseWriter, r *http.Request, path string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", path)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// flashes are fixed messages selected by ?ok=, so nothing user-supplied is
// reflected into the page.
var flashes = map[string]string{
	"created":  "Proje oluşturuldu. Deploy için repoya /webhooks/github adresine giden bir push webhook'u ekle.",
	"rollback": "Canlı site artık seçtiğin sürümü gösteriyor.",
	// Faz 16–19 screens.
	"promoted":       "Production'a taşınıyor: aynı imajla yeni bir deploy kuyruğa alındı.",
	"promoted-alias": "Canlı site artık seçtiğin deploy'u gösteriyor.",
	"redeploy":       "Yeni deploy kuyruğa alındı.",
	"canceled":       "Deploy iptal edildi.",
	"cancel":         "İptal istendi; deploy birkaç saniye içinde durur.",
	"settings":       "Build ayarları kaydedildi. Bir sonraki deploy'da geçerli olur.",
	"hook-deleted":   "Deploy hook'u silindi.",
	"env":            "Kaydedildi. Yeni değerler bir sonraki deploy'da geçerli olur.",
	"domain":         "Alan adları güncellendi.",
	"team":           "Ekip oluşturuldu.",
	"member":         "Ekip üyeleri güncellendi.",
	"revoked":        "Token iptal edildi.",
	// Faz 15.
	"imported":       "Proje oluşturuldu, ilk deploy başladı.",
	"imported-idle":  "Proje oluşturuldu. Deploy için canlı branch'e push et.",
	"github":         "GitHub bağlandı. Repoların aşağıda.",
	"github-updated": "GitHub ayarları kaydedildi. Repo değişiklikleri birkaç saniye içinde burada görünür.",
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.render(w, r, status, "error", statusTitle(status), nil, msg)
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("web: internal error", "path", r.URL.Path, "err", err)
	s.errorPage(w, r, http.StatusInternalServerError, "Bir şeyler ters gitti. Ayrıntılar sunucu logunda.")
}

// ---- login ----

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Auth.Session(r); err == nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", "Giriş yap", s.loginData(safeNext(r.URL.Query().Get("next"))),
		loginErrors[r.URL.Query().Get("error")])
}

// loginData selects the login method: GitHub, or the token form in dev mode.
func (s *Server) loginData(next string) map[string]any {
	return map[string]any{"Next": next, "GitHub": s.GitHub != nil, "TokenLogin": s.Auth.TokenLogin()}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	next := safeNext(r.PostFormValue("next"))
	if !s.Auth.TokenLogin() {
		http.Error(w, "token ile giriş kapalı: GitHub ile giriş yap", http.StatusNotFound)
		return
	}
	if !auth.SameOrigin(r) {
		http.Error(w, "başka siteden gelen giriş isteği reddedildi", http.StatusForbidden)
		return
	}
	if !s.Sessions.CheckToken(strings.TrimSpace(r.PostFormValue("token"))) {
		s.Log.Warn("web: failed login", "remote", r.RemoteAddr)
		s.render(w, r, http.StatusUnauthorized, "login", "Giriş yap", s.loginData(next), "Geçersiz API token'ı.")
		return
	}
	s.Sessions.Issue(w, r)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.Sessions.Clear(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// safeNext only allows local paths, so ?next= cannot redirect off-site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// ---- apps ----

type appRow struct {
	store.App
	Team          string
	ProductionURL string
	Latest        *store.Deployment
}

func (s *Server) appsPage(w http.ResponseWriter, r *http.Request) {
	s.renderApps(w, r, http.StatusOK, "", nil)
}

func (s *Server) renderApps(w http.ResponseWriter, r *http.Request, status int, errMsg string, form map[string]string) {
	me, _ := auth.From(r.Context())
	apps, err := s.Auth.Apps(r.Context(), me)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	teams, err := s.Auth.Teams(r.Context(), me)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	slugs := map[int64]string{}
	var writable []store.Team
	for _, t := range teams {
		slugs[t.ID] = t.Slug
		if store.RoleAllows(t.Role, store.RoleMember) {
			writable = append(writable, t)
		}
	}
	latest, err := s.Store.LatestDeployments(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	rows := make([]appRow, 0, len(apps))
	for _, a := range apps {
		row := appRow{App: a, Team: slugs[a.TeamID], ProductionURL: naming.URL(s.Scheme, naming.ProductionHost(a.Name, s.Domain))}
		if d, ok := latest[a.ID]; ok {
			row.Latest = &d
		}
		rows = append(rows, row)
	}
	if form == nil {
		form = map[string]string{"Branch": "main"}
	}
	s.render(w, r, status, "apps", "Projeler", map[string]any{"Apps": rows, "Form": form, "Teams": writable}, errMsg)
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	repo := strings.TrimSpace(r.PostFormValue("repo"))
	branch := strings.TrimSpace(r.PostFormValue("production_branch"))
	if branch == "" {
		branch = "main"
	}
	teamSlug := strings.TrimSpace(r.PostFormValue("team"))
	form := map[string]string{"Name": name, "Repo": repo, "Branch": branch, "Team": teamSlug, "Open": "1"}
	switch {
	case !naming.ValidAppName(name):
		s.renderApps(w, r, http.StatusBadRequest,
			"Ad 2-31 karakter olmalı: a-z harfleri, rakamlar ve '-'; harfle başlamalı.", form)
		return
	case !api.ValidRepo(repo):
		s.renderApps(w, r, http.StatusBadRequest, `Repo "sahip/repo" biçiminde olmalı.`, form)
		return
	}
	me, _ := auth.From(r.Context())
	team, err := s.Auth.TeamForApps(r.Context(), me, teamSlug)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, auth.ErrForbidden) {
		s.renderApps(w, r, http.StatusForbidden, "Proje eklemek için ekipte üye rolü gerekir.", form)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	_, err = s.Store.CreateAppInTeam(r.Context(), team.ID, name, repo, branch)
	if errors.Is(err, store.ErrConflict) {
		s.renderApps(w, r, http.StatusConflict, "Bu ad ya da repo ile bir proje zaten var.", form)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("web: app created", "app", name, "repo", repo)
	http.Redirect(w, r, "/apps/"+name+"?ok=created", http.StatusSeeOther)
}

// ---- deployment detail ----

func (s *Server) deploymentPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.errorPage(w, r, http.StatusNotFound, "Deploy bulunamadı.")
		return
	}
	d, err := s.Store.GetDeployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Deploy bulunamadı.")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	app, err := s.Store.GetAppByName(r.Context(), d.AppName)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !s.checkTeam(w, r, app.TeamID, store.RoleViewer, "Deploy bulunamadı.") {
		return
	}
	s.render(w, r, http.StatusOK, "deployment", fmt.Sprintf("%s · %s", d.AppName, naming.ShortSHA(d.CommitSHA)),
		map[string]any{
			"D":           d,
			"URL":         naming.URL(s.Scheme, d.Host(s.Domain)),
			"RuntimeLogs": s.RuntimeLogs,
		}, "")
}
