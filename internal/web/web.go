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
	"sort"
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

	pages map[string]*template.Template
}

var funcs = template.FuncMap{
	"short": naming.ShortSHA,
	"firstLine": func(s string) string {
		s, _, _ = strings.Cut(s, "\n")
		return s
	},
	"ago":  ago,
	"time": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
	"active": func(status string) bool {
		return status == store.StatusQueued || status == store.StatusBuilding || status == store.StatusDeploying
	},
	"duration": func(d store.Deployment) string {
		if d.StartedAt == nil || d.FinishedAt == nil {
			return ""
		}
		return d.FinishedAt.Sub(*d.StartedAt).Round(time.Second).String()
	},
}

func (s *Server) Handler() http.Handler {
	s.pages = map[string]*template.Template{}
	if s.Auth == nil {
		s.Auth = &auth.Authenticator{Store: s.Store, Sessions: s.Sessions, Log: s.Log}
	}
	for _, p := range []string{"login", "apps", "app", "deployment", "error", "teams", "team", "tokens", "redirect", "import"} {
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
	mux.Handle("GET /apps/{name}/deployments", s.authed(s.deploymentsPartial))
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
		s.errorPage(w, r, http.StatusNotFound, "Page not found.")
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
				http.Error(w, "invalid bearer token", http.StatusUnauthorized)
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
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			if !s.Sessions.CheckCSRF(r) {
				http.Error(w, "invalid CSRF token or cross-origin request", http.StatusForbidden)
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
		Title: title, CSRF: s.Sessions.CSRFToken(r), Domain: s.Domain, Data: data, Error: errMsg,
		Flash: flashes[r.URL.Query().Get("ok")],
	}
	p.Nonce, _ = r.Context().Value(nonceKey).(string)
	p.Me, p.LoggedIn = auth.From(r.Context())
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		s.Log.Error("render", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// partial renders a fragment for htmx.
func (s *Server) partial(w http.ResponseWriter, r *http.Request, status int, name string, data any, envErr string) {
	var buf bytes.Buffer
	if err := s.pages[name].Execute(&buf, page{Data: data, CSRF: s.Sessions.CSRFToken(r), EnvError: envErr}); err != nil {
		s.Log.Error("render", "partial", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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
	"created":  "App created. Point a GitHub push webhook at /webhooks/github to deploy it.",
	"rollback": "Production now serves the selected deployment.",
	"env":      "Environment updated. New values apply to the next deployment.",
	"domain":   "Custom domains updated.",
	"team":     "Team created.",
	"member":   "Team members updated.",
	"revoked":  "Token revoked.",
	// Faz 15.
	"imported":       "App imported from GitHub. Its first deployment is queued.",
	"imported-idle":  "App imported. Push to its production branch to deploy it.",
	"github":         "GitHub App installation linked to your team. Its repositories are listed below.",
	"github-updated": "GitHub App settings saved. Repository changes appear once GitHub notifies the platform.",
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.render(w, r, status, "error", http.StatusText(status), nil, msg)
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("web: internal error", "path", r.URL.Path, "err", err)
	s.errorPage(w, r, http.StatusInternalServerError, "Something went wrong. The server log has details.")
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ---- login ----

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Auth.Session(r); err == nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", "Log in", s.loginData(safeNext(r.URL.Query().Get("next"))),
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
		http.Error(w, "token login is disabled: sign in with GitHub", http.StatusNotFound)
		return
	}
	if !auth.SameOrigin(r) {
		http.Error(w, "cross-origin login rejected", http.StatusForbidden)
		return
	}
	if !s.Sessions.CheckToken(strings.TrimSpace(r.PostFormValue("token"))) {
		s.Log.Warn("web: failed login", "remote", r.RemoteAddr)
		s.render(w, r, http.StatusUnauthorized, "login", "Log in", s.loginData(next), "Invalid API token.")
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
	s.render(w, r, status, "apps", "Apps", map[string]any{"Apps": rows, "Form": form, "Teams": writable}, errMsg)
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
			"Name must be 2-31 chars: lowercase letters, digits and '-', starting with a letter.", form)
		return
	case !api.ValidRepo(repo):
		s.renderApps(w, r, http.StatusBadRequest, `Repository must look like "owner/repo".`, form)
		return
	}
	me, _ := auth.From(r.Context())
	team, err := s.Auth.TeamForApps(r.Context(), me, teamSlug)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, auth.ErrForbidden) {
		s.renderApps(w, r, http.StatusForbidden, "You need the member role on a team to create apps there.", form)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	_, err = s.Store.CreateAppInTeam(r.Context(), team.ID, name, repo, branch)
	if errors.Is(err, store.ErrConflict) {
		s.renderApps(w, r, http.StatusConflict, "An app with this name or repository already exists.", form)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("web: app created", "app", name, "repo", repo)
	http.Redirect(w, r, "/apps/"+name+"?ok=created", http.StatusSeeOther)
}

// ---- app detail ----

type deploymentRow struct {
	store.Deployment
	URL         string
	Production  bool
	CanRollback bool
	AliasesHere []string
}

type appDetail struct {
	App           store.App
	ProductionURL string
	Aliases       []aliasRow
	Deployments   []deploymentRow
	Active        bool // something is in progress: the table polls
	EnvKeys       []string
	HasProduction bool
	Domains       []api.DomainView
	// Faz 13: the caller's role on the app's team; CanWrite = member+.
	Team     store.Team
	Role     string
	CanWrite bool
}

type aliasRow struct {
	store.Alias
	URL string
	SHA string
}

// loadApp loads the {name} app and checks the caller's team role: viewer
// for GET, member for POST. Apps of other teams look like missing ones.
func (s *Server) loadApp(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	app, err := s.Store.GetAppByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "App not found.")
		return app, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return app, false
	}
	need := store.RoleMember
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		need = store.RoleViewer
	}
	if !s.checkTeam(w, r, app.TeamID, need, "App not found.") {
		return app, false
	}
	return app, true
}

func (s *Server) appDetail(ctx context.Context, app store.App) (appDetail, error) {
	v := appDetail{App: app, ProductionURL: naming.URL(s.Scheme, naming.ProductionHost(app.Name, s.Domain))}
	me, _ := auth.From(ctx)
	role, err := s.Auth.TeamRole(ctx, me, app.TeamID)
	if err != nil {
		return v, err
	}
	if v.Team, err = s.Store.GetTeam(ctx, app.TeamID); err != nil {
		return v, err
	}
	v.Role, v.CanWrite = role, store.RoleAllows(role, store.RoleMember)
	aliases, err := s.Store.ListAliases(ctx, app.ID)
	if err != nil {
		return v, err
	}
	deps, err := s.Store.ListDeployments(ctx, app.ID, 50)
	if err != nil {
		return v, err
	}
	env, err := s.Store.AppEnv(ctx, app.ID)
	if err != nil {
		return v, err
	}
	shas := map[int64]string{}
	for _, d := range deps {
		shas[d.ID] = d.CommitSHA
	}
	hosts := map[int64][]string{}
	var prodID int64
	for _, a := range aliases {
		v.Aliases = append(v.Aliases, aliasRow{Alias: a, URL: naming.URL(s.Scheme, a.Hostname), SHA: shas[a.DeploymentID]})
		hosts[a.DeploymentID] = append(hosts[a.DeploymentID], a.Hostname)
		if a.Kind == store.AliasProduction {
			prodID = a.DeploymentID
			v.HasProduction = true
		}
	}
	for _, d := range deps {
		v.Deployments = append(v.Deployments, deploymentRow{
			Deployment:  d,
			URL:         naming.URL(s.Scheme, naming.DeploymentHost(d.CommitSHA, d.AppName, s.Domain)),
			Production:  d.ID == prodID,
			CanRollback: v.CanWrite && v.HasProduction && d.ID != prodID && d.Status == store.StatusReady,
			AliasesHere: hosts[d.ID],
		})
		if !d.Finished() {
			v.Active = true
		}
	}
	for k := range env {
		v.EnvKeys = append(v.EnvKeys, k)
	}
	sort.Strings(v.EnvKeys)
	v.Domains, err = s.domainViews(ctx, app)
	return v, err
}

func (s *Server) appPage(w http.ResponseWriter, r *http.Request) {
	s.renderApp(w, r, http.StatusOK, "")
}

func (s *Server) renderApp(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	v, err := s.appDetail(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, r, status, "app", app.Name, v, errMsg)
}

// deploymentsPartial is polled by htmx while a deployment is in progress.
func (s *Server) deploymentsPartial(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	v, err := s.appDetail(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.partial(w, r, http.StatusOK, "deployments", v, "")
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("deployment_id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderApp(w, r, http.StatusBadRequest, "Invalid deployment.")
		return
	}
	_, err = s.Store.Rollback(r.Context(), app.ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.renderApp(w, r, http.StatusNotFound, "Deployment or production alias not found for this app.")
		return
	case errors.Is(err, store.ErrNotReady):
		s.renderApp(w, r, http.StatusConflict, "Only ready deployments can receive production traffic.")
		return
	case errors.Is(err, store.ErrRetired):
		s.renderApp(w, r, http.StatusConflict,
			"This deployment is retired and no longer runs; push its commit again to redeploy it.")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("rollback", "app", app.Name, "deployment", id, "via", "web")
	if s.Router != nil {
		if err := s.Router.SyncApp(r.Context(), app.Name); err != nil {
			s.Log.Error("rollback: route sync", "app", app.Name, "err", err)
			s.renderApp(w, r, http.StatusBadGateway,
				"Rollback saved, but updating the router failed; it is retried automatically.")
			return
		}
	}
	redirect(w, r, "/apps/"+app.Name+"?ok=rollback")
}

// ---- environment ----

func (s *Server) setEnv(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PostFormValue("key"))
	value := r.PostFormValue("value")
	s.changeEnv(w, r, key, &value)
}

func (s *Server) deleteEnv(w http.ResponseWriter, r *http.Request) {
	s.changeEnv(w, r, r.PostFormValue("key"), nil)
}

func (s *Server) changeEnv(w http.ResponseWriter, r *http.Request, key string, value *string) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	status, msg := http.StatusOK, ""
	if err := api.CheckEnvVar(key, value); err != nil {
		status, msg = http.StatusBadRequest, err.Error()
	} else if err := s.Store.UpdateAppEnv(r.Context(), app.ID, map[string]*string{key: value}); err != nil {
		s.internalError(w, r, err)
		return
	} else {
		s.Log.Info("env updated", "app", app.Name, "changed", 1, "via", "web")
	}
	switch {
	case isHTMX(r):
		v, err := s.appDetail(r.Context(), app)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		s.partial(w, r, status, "env", v, msg)
		return
	case msg != "":
		s.renderApp(w, r, status, msg)
		return
	}
	http.Redirect(w, r, "/apps/"+app.Name+"?ok=env#env", http.StatusSeeOther)
}

// ---- deployment detail ----

func (s *Server) deploymentPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.errorPage(w, r, http.StatusNotFound, "Deployment not found.")
		return
	}
	d, err := s.Store.GetDeployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Deployment not found.")
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
	if !s.checkTeam(w, r, app.TeamID, store.RoleViewer, "Deployment not found.") {
		return
	}
	s.render(w, r, http.StatusOK, "deployment", fmt.Sprintf("%s · %s", d.AppName, naming.ShortSHA(d.CommitSHA)),
		map[string]any{
			"D":           d,
			"URL":         naming.URL(s.Scheme, naming.DeploymentHost(d.CommitSHA, d.AppName, s.Domain)),
			"RuntimeLogs": s.RuntimeLogs,
		}, "")
}
