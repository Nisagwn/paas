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
	"github.com/nisagwn/paas/internal/appdelete"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/protect"
	"github.com/nisagwn/paas/internal/ratelimit"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/terminal"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	Store *store.Store
	// Router is nil when nothing is routed (dry run).
	Router   api.Router
	Sessions *auth.Sessions
	Domain   string
	// PublicURL is where the UI is reachable (PAAS_PUBLIC_URL); the landing
	// page uses it for its Open Graph and canonical links. Empty omits them.
	PublicURL string
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
	// Faz 20 (processes.go): status, scaling and cron runs of an app's
	// processes; nil (dry-run deployer) shows the configuration only.
	Processes api.ProcessCluster
	// Faz 24 (protection.go): deployment protection's login page and the
	// Koruma section; nil hides the login page.
	Protect *protect.Guard

	// Faz 25 (comments.go): mirrors comment threads to pull requests; nil
	// keeps them in paas only.
	Comments api.CommentSync
	// Faz 27 (terminal.go): browser terminal; nil (PAAS_TERMINAL=false or
	// the dry-run deployer) removes its routes and buttons.
	Terminal *terminal.Service

	// Faz 26 (notifications.go): the team's Bildirimler page and
	// production change events; nil stores channels but sends nothing.
	Notifications *notify.Service

	// Faz 23 (plans.go), all optional: plan enforcement (nil: no limits),
	// rate limits, scaling suspended teams to zero, the legal pages.
	Plans     *plans.Guard
	Limits    *ratelimit.Set
	Suspender api.Suspender
	Legal     Legal

	// Faz 28 (delete.go): project deletion; nil deletes without a cluster
	// (the rows only, as with the dry-run deployer).
	Deleter *appdelete.Service

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
		"deploys", "analytics", "settings", "databases", "review", "usage", "admin", "terms", "privacy", "landing"} {
		s.pages[p] = template.Must(template.New("").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/processes.html", "templates/rollout.html", "templates/"+p+".html"))
	}
	partials := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/partials.html", "templates/processes.html", "templates/rollout.html"))
	s.pages["deployments"] = partials.Lookup("deployments")
	s.pages["env"] = partials.Lookup("env")
	s.pages["domains"] = partials.Lookup("domains")
	s.pages["processes"] = partials.Lookup("processes")
	s.protectionTemplates() // Faz 24
	s.authTemplates()       // sign-in and sign-up pages (signup.go)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.limitSignIn(s.login))
	mux.Handle("POST /logout", s.authed(s.logout))
	mux.HandleFunc("GET /{$}", s.home(s.authed(s.appsPage))) // landing page without a session (landing.go)
	mux.Handle("POST /apps", s.authed(s.createApp))
	mux.Handle("GET /apps/{name}", s.authed(s.appPage))
	// Faz 16–19 screens: the app's tabs (app.go, controls.go, analytics.go). The
	// deployments tab is also the htmx fragment polled while one runs.
	mux.Handle("GET /apps/{name}/deployments", s.authed(s.deploymentsPage))
	mux.Handle("GET /apps/{name}/analytics", s.authed(s.analyticsPage))
	mux.Handle("GET /apps/{name}/settings", s.authed(s.settingsPage))
	mux.Handle("POST /apps/{name}/settings", s.authed(s.saveSettings))
	mux.Handle("POST /apps/{name}/delete", s.authed(s.deleteApp)) // Faz 28 (delete.go)
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
	// Faz 20: processes (processes.go).
	mux.Handle("GET /apps/{name}/processes", s.authed(s.processesPartial))
	mux.Handle("POST /apps/{name}/processes/scale", s.authed(s.scaleProcess))
	mux.Handle("POST /apps/{name}/crons/run", s.authed(s.runCron))
	// Faz 21: rollout panel actions and settings (rollouts.go).
	mux.Handle("POST /apps/{name}/rollout/{action}", s.authed(s.rolloutAction))
	mux.Handle("POST /apps/{name}/rollout-settings", s.authed(s.rolloutSettings))
	// Faz 22: the Veritabanları tab (databases.go).
	mux.Handle("GET /apps/{name}/databases", s.authed(s.databasesPage))
	mux.Handle("POST /apps/{name}/databases", s.authed(s.createDatabase))
	mux.Handle("POST /apps/{name}/databases/{addon}/settings", s.authed(s.updateDatabase))
	mux.Handle("POST /apps/{name}/databases/{addon}/reveal", s.authed(s.revealDatabase))
	mux.Handle("POST /apps/{name}/databases/{addon}/rotate", s.authed(s.rotateDatabase))
	mux.Handle("POST /apps/{name}/databases/{addon}/branches/reset", s.authed(s.resetDatabaseBranch))
	mux.Handle("POST /apps/{name}/databases/{addon}/backups", s.authed(s.backupDatabase))
	mux.Handle("POST /apps/{name}/databases/{addon}/backups/restore", s.authed(s.restoreDatabase))
	mux.Handle("POST /apps/{name}/databases/{addon}/delete", s.authed(s.deleteDatabase))
	// Faz 24: deployment protection (protection.go).
	s.protectionRoutes(mux)
	mux.Handle("GET /deployments/{id}", s.authed(s.deploymentPage))
	// Faz 25: review page and preview comments (comments.go).
	s.commentRoutes(mux)
	s.terminalRoutes(mux)  // Faz 27 (terminal.go)
	s.projectUXRoutes(mux) // project and deployment screens (project_ux.go)
	// Faz 13: GitHub login, teams and personal tokens (oauth.go, users.go).
	mux.HandleFunc("GET /auth/github", s.limitSignIn(s.githubStart))
	mux.HandleFunc("GET /auth/github/callback", s.limitSignIn(s.githubCallback))
	mux.HandleFunc("GET /signup", s.signupPage) // signup.go
	mux.Handle("GET /teams", s.authed(s.teamsPage))
	mux.Handle("POST /teams", s.authed(s.createTeam))
	mux.Handle("GET /teams/{slug}", s.authed(s.teamPage))
	mux.Handle("POST /teams/{slug}/members", s.authed(s.setMember))
	mux.Handle("POST /teams/{slug}/members/remove", s.authed(s.removeMember))
	s.notificationRoutes(mux) // Faz 26: /teams/{slug}/notifications
	mux.Handle("GET /tokens", s.authed(s.tokensPage))
	mux.Handle("POST /tokens", s.authed(s.createToken))
	mux.Handle("POST /tokens/revoke", s.authed(s.revokeToken))
	// Faz 15: GitHub App install, setup URL and import (github.go). The
	// setup URL is a redirect from github.com and carries no session.
	mux.Handle("GET /github/install", s.authed(s.githubInstall))
	mux.HandleFunc("GET /github/setup", s.githubSetup)
	mux.Handle("GET /import", s.authed(s.importPage))
	mux.Handle("POST /import", s.authed(s.importApp))
	// Faz 23: usage, admin and legal pages (plans.go).
	mux.Handle("GET /usage", s.authed(s.usagePage))
	mux.Handle("GET /admin", s.authed(s.requireAdmin(s.adminPage)))
	mux.Handle("POST /admin/teams/{slug}/plan", s.authed(s.requireAdmin(s.adminSetPlan)))
	mux.Handle("POST /admin/teams/{slug}/suspend", s.authed(s.requireAdmin(s.adminSuspend)))
	mux.Handle("POST /admin/teams/{slug}/unsuspend", s.authed(s.requireAdmin(s.adminUnsuspend)))
	mux.Handle("POST /admin/users/suspend", s.authed(s.requireAdmin(s.adminSuspendUser)))
	mux.Handle("POST /admin/users/unsuspend", s.authed(s.requireAdmin(s.adminUnsuspendUser)))
	mux.HandleFunc("GET /terms", s.termsPage)
	mux.HandleFunc("GET /privacy", s.privacyPage)
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
		if errors.Is(err, auth.ErrSuspended) { // Faz 23 (plans.go)
			s.suspendedSession(w, r)
			return
		}
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
			if !s.allowWrite(w, r, p) { // Faz 23 rate limit (plans.go)
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
	// PlatformAdmin shows the admin link (Faz 23).
	PlatformAdmin bool
	// Page is the template name; Nav the header of signed-in pages (nav.go).
	Page string
	Nav  navData
	// Signup: anyone may create an account (PAAS_OPEN_SIGNUP); the signed-out
	// header shows "Kaydol" only then.
	Signup bool
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name, title string, data any, errMsg string) {
	p := page{
		Title: tr(title), CSRF: s.Sessions.CSRFToken(r), Domain: s.Domain, Data: data, Error: tr(errMsg),
		Flash: flashes[r.URL.Query().Get("ok")],
	}
	if f, ok := deletedFlash(r); ok { // Faz 28
		p.Flash = f
	}
	p.Nonce, _ = r.Context().Value(nonceKey).(string)
	p.Me, p.LoggedIn = auth.From(r.Context())
	p.PlatformAdmin = p.LoggedIn && s.Auth.PlatformAdmin(p.Me)
	p.Page = name
	p.Signup = s.openSignup()
	if p.LoggedIn {
		p.Nav = s.nav(r, p.Me)
	}
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
	// Faz 21.
	"rollout":          "Yayın güncellendi.",
	"rollout-settings": "Yayın ayarları kaydedildi. Bir sonraki production deploy'unda geçerli olur.",
	// Faz 22.
	"db-created": "Veritabanı hazırlanıyor. Hazır olunca bağlantı değişkenleri sonraki deploy'lara eklenir; mevcut deploy'lar için yeniden deploy et.",
	"db-saved":   "Veritabanı ayarları kaydedildi. Önizleme ve anonimleştirme ayarları sonraki kopyalarda geçerli olur.",
	"db-rotate":  "Yeni şifre bir dakika içinde uygulanır.",
	"db-reset":   "Kopya production'dan yeniden alınıyor.",
	"db-backup":  "Yedek alınıyor.",
	"db-restore": "Geri yükleme başladı; production veritabanı seçtiğin yedekle değiştirilecek.",
	"db-deleted": "Veritabanı siliniyor; verileri ve yedekleri de silinecek.",
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.render(w, r, status, "error", statusTitle(status), errorData(status), msg)
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if status, msg, ok := limitError(err); ok { // Faz 23: a plan refusal
		s.errorPage(w, r, status, msg)
		return
	}
	s.Log.Error("web: internal error", "path", r.URL.Path, "err", err)
	s.errorPage(w, r, http.StatusInternalServerError, "Bir şeyler ters gitti. Ayrıntılar sunucu logunda.")
}

// ---- login ----

// The sign-in and sign-up pages live in login.go and signup.go.

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.Sessions.Clear(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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
			"Processes":   s.processNames(r.Context(), d), // Faz 20: runtime log selector
			"Terminal":    s.terminalAllowed(r, app, d),   // Faz 27
			"X":           s.deploymentUX(r, app, d, s.terminalAllowed(r, app, d)),
		}, "")
}
