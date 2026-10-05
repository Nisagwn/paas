// Package api exposes the paas control-plane HTTP API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/webhook"
)

const maxWebhookBody = 5 << 20 // GitHub caps payloads at 25 MB; pushes are far smaller.

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Router pushes an app's aliases to the ingress layer (routing.Syncer).
type Router interface {
	SyncApp(ctx context.Context, app string) error
}

type Server struct {
	Store *store.Store
	// Router is nil when nothing is routed (dry run).
	Router Router
	Domain string
	// Scheme of app URLs in responses; empty means "https".
	Scheme        string
	APIToken      string
	WebhookSecret string
	Log           *slog.Logger

	// Faz 5, all optional.
	// Sessions lets browsers authenticate GET requests with the web UI's
	// session cookie (EventSource cannot send an Authorization header).
	Sessions *auth.Sessions
	// Events wakes live log streams; nil falls back to polling.
	Events Notifier
	// RuntimeLogs streams pod logs; nil (dry-run deployer) answers 501.
	RuntimeLogs RuntimeLogs
	// UI is mounted at "/" (internal/web).
	UI http.Handler
	// cookieless: no Sessions were given, so session cookies are ignored.
	cookieless bool
	// Stream tunes the SSE endpoints (tests shorten it).
	Stream StreamTiming
	// Cleanup is kicked after a branch deletion (cleanup.Collector); nil
	// leaves route sync and object deletion to the periodic sweep.
	Cleanup interface{ Kick(app string) }
	// Domains checks a custom domain on demand (Faz 12); nil leaves new
	// domains pending until the verifier loop runs.
	Domains DomainChecker

	// Faz 13: resolves callers and roles (access.go). Nil builds one from
	// Store, Sessions and APIToken (legacy admin token only, no OAuth).
	Auth *auth.Authenticator

	// Faz 15: reads repositories through the GitHub App for imports (first
	// deployment, default branch); nil means no GitHub App is configured,
	// and imports create the app without a deployment.
	GitHubApp RepoInspector
}

func (s *Server) Handler() http.Handler {
	if s.Sessions == nil {
		s.Sessions = auth.New(s.APIToken)
		s.cookieless = true
	}
	if s.Auth == nil {
		s.Auth = &auth.Authenticator{Store: s.Store, Sessions: s.Sessions, Log: s.Log}
	}
	viewer, member := store.RoleViewer, store.RoleMember
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /webhooks/github", s.githubWebhook)
	// Faz 17: deploy hooks authenticate with the secret in the path only,
	// so they bypass s.authenticate (more specific than "/api/").
	mux.HandleFunc("POST /api/hooks/deploy/{token}", s.triggerHook)

	api := http.NewServeMux()
	api.HandleFunc("POST /api/apps", s.createApp)
	api.HandleFunc("GET /api/apps", s.listApps)
	// Faz 15: repositories of the GitHub App installations of the caller's
	// teams, and importing one (github_import.go; roles checked there).
	api.HandleFunc("GET /api/github/repos", s.listImportable)
	api.HandleFunc("POST /api/apps/import", s.importApp)
	// Per-app endpoints: requireApp checks the caller's team role (access.go).
	api.HandleFunc("GET /api/apps/{name}", s.requireApp(viewer, s.getApp))
	api.HandleFunc("GET /api/apps/{name}/deployments", s.requireApp(viewer, s.listDeployments))
	api.HandleFunc("POST /api/apps/{name}/rollback", s.requireApp(member, s.rollback))
	api.HandleFunc("GET /api/apps/{name}/env", s.requireApp(viewer, s.getEnv))
	api.HandleFunc("PUT /api/apps/{name}/env", s.requireApp(member, s.putEnv))
	api.HandleFunc("GET /api/apps/{name}/domains", s.requireApp(viewer, s.listDomains))
	api.HandleFunc("POST /api/apps/{name}/domains", s.requireApp(member, s.addDomain))
	api.HandleFunc("POST /api/apps/{name}/domains/{hostname}/verify", s.requireApp(member, s.verifyDomain))
	api.HandleFunc("DELETE /api/apps/{name}/domains/{hostname}", s.requireApp(member, s.deleteDomain))
	api.HandleFunc("GET /api/apps/{name}/scale-to-zero", s.requireApp(viewer, s.getScaleToZero))
	api.HandleFunc("PUT /api/apps/{name}/scale-to-zero", s.requireApp(member, s.putScaleToZero))
	// Faz 17: promote, redeploy, deploy hooks (controls.go). Hooks are
	// secrets, so even listing them needs member.
	api.HandleFunc("POST /api/apps/{name}/promote", s.requireApp(member, s.promote))
	api.HandleFunc("POST /api/apps/{name}/deployments/{id}/redeploy", s.requireApp(member, s.redeploy))
	api.HandleFunc("GET /api/apps/{name}/hooks", s.requireApp(member, s.listHooks))
	api.HandleFunc("POST /api/apps/{name}/hooks", s.requireApp(member, s.createHook))
	api.HandleFunc("DELETE /api/apps/{name}/hooks/{id}", s.requireApp(member, s.deleteHook))
	// Checks member on the deployment's app (lookupDeploymentAs).
	api.HandleFunc("POST /api/deployments/{id}/cancel", s.cancelDeployment)
	// Not wrapped: it answers 501 without a deployer first; lookupApp checks viewer.
	api.HandleFunc("GET /api/apps/{name}/deployments/{id}/runtime-logs", s.runtimeLogs)
	// Deployment endpoints check the deployment's app (lookupDeployment).
	api.HandleFunc("GET /api/deployments/{id}", s.getDeployment)
	api.HandleFunc("GET /api/deployments/{id}/logs", s.deploymentLogs)
	api.HandleFunc("GET /api/deployments/{id}/logs/stream", s.streamLogs)
	// Faz 13: caller, teams, members and personal tokens (teams.go).
	api.HandleFunc("GET /api/me", s.me)
	api.HandleFunc("GET /api/teams", s.listTeams)
	api.HandleFunc("POST /api/teams", s.createTeam)
	api.HandleFunc("GET /api/teams/{slug}", s.getTeam)
	api.HandleFunc("PUT /api/teams/{slug}/members/{login}", s.setMember)
	api.HandleFunc("DELETE /api/teams/{slug}/members/{login}", s.removeMember)
	api.HandleFunc("GET /api/tokens", s.listTokens)
	api.HandleFunc("POST /api/tokens", s.createToken)
	api.HandleFunc("DELETE /api/tokens/{id}", s.revokeToken)
	mux.Handle("/api/", s.authenticate(api))
	if s.UI != nil {
		mux.Handle("/", s.UI)
	}

	return s.logRequests(mux)
}

// ---- middleware ----

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"dur_ms", time.Since(start).Milliseconds())
	})
}

// ---- handlers ----

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type createAppRequest struct {
	Name             string `json:"name"`
	Repo             string `json:"repo"`
	ProductionBranch string `json:"production_branch"`
	// Team slug; empty picks the caller's first team where it is a member.
	Team string `json:"team"`
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	var req createAppRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !naming.ValidAppName(req.Name) {
		writeError(w, http.StatusBadRequest,
			"name must be 2-31 chars: lowercase letters, digits, '-', starting with a letter")
		return
	}
	if !repoRe.MatchString(req.Repo) {
		writeError(w, http.StatusBadRequest, `repo must look like "owner/repo"`)
		return
	}
	if req.ProductionBranch == "" {
		req.ProductionBranch = "main"
	}
	team, err := s.Auth.TeamForApps(r.Context(), principal(r), req.Team)
	switch {
	case errors.Is(err, store.ErrNotFound) && req.Team == "":
		writeError(w, http.StatusForbidden, "you are not a member of any team that can create apps")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "team not found")
		return
	case errors.Is(err, auth.ErrForbidden):
		writeError(w, http.StatusForbidden, "creating apps needs the member role on the team")
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	app, err := s.Store.CreateAppInTeam(r.Context(), team.ID, req.Name, req.Repo, req.ProductionBranch)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "an app with this name or repo already exists")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.appView(app, nil))
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.Auth.Apps(r.Context(), principal(r))
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := make([]appView, 0, len(apps))
	for _, a := range apps {
		out = append(out, s.appView(a, nil))
	}
	writeJSON(w, http.StatusOK, out)
}

type appView struct {
	store.App
	ProductionURL string        `json:"production_url"`
	Aliases       []store.Alias `json:"aliases,omitempty"`
}

func (s *Server) appView(a store.App, aliases []store.Alias) appView {
	return appView{App: a, ProductionURL: naming.URL(s.Scheme, naming.ProductionHost(a.Name, s.Domain)), Aliases: aliases}
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	aliases, err := s.Store.ListAliases(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.appView(app, aliases))
}

type deploymentView struct {
	store.Deployment
	URL string `json:"url"`
	// Sleeping: scaled to zero; the next request wakes it (Faz 11).
	Sleeping bool `json:"sleeping"`
}

func (s *Server) deploymentView(d store.Deployment) deploymentView {
	return deploymentView{Deployment: d, URL: naming.URL(s.Scheme, d.Host(s.Domain)),
		Sleeping: d.Status == store.StatusReady && d.SleepingSince != nil}
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	deps, err := s.Store.ListDeployments(r.Context(), app.ID, limit)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := make([]deploymentView, 0, len(deps))
	for _, d := range deps {
		out = append(out, s.deploymentView(d))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDeployment(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.deploymentView(d))
}

// deploymentLogs returns lines after ?after=<id>; streamLogs is the live variant.
func (s *Server) deploymentLogs(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDeployment(w, r)
	if !ok {
		return
	}
	id := d.ID
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = n
	}
	lines, err := s.Store.Logs(r.Context(), id, after, 1000)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lines)
}

type rollbackRequest struct {
	DeploymentID int64 `json:"deployment_id"`
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var req rollbackRequest
	if err := decodeJSON(r, &req); err != nil || req.DeploymentID <= 0 {
		writeError(w, http.StatusBadRequest, "body must be {\"deployment_id\": <id>}")
		return
	}
	alias, err := s.Store.Rollback(r.Context(), app.ID, req.DeploymentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "deployment or production alias not found for this app")
	case errors.Is(err, store.ErrRetired):
		writeError(w, http.StatusConflict,
			"deployment is retired: its Kubernetes objects were removed; push the commit again to redeploy it")
	case errors.Is(err, store.ErrNotReady):
		writeError(w, http.StatusConflict, "only ready deployments can receive production traffic")
	case err != nil:
		s.internalError(w, err)
	default:
		s.Log.Info("rollback", "app", app.Name, "deployment", req.DeploymentID)
		// The alias already points at the old deployment in the database, so
		// a failed sync is retried by the reconcile loop; report it anyway.
		if s.Router != nil {
			if err := s.Router.SyncApp(r.Context(), app.Name); err != nil {
				s.Log.Error("rollback: route sync", "app", app.Name, "err", err)
				writeError(w, http.StatusBadGateway,
					"rollback saved, but updating the router failed (retried automatically): "+err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, alias)
	}
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,254}$`)

const maxEnvValue = 32 << 10 // all values of an app share one 1 MiB Secret

type envView struct {
	// Variable names in any environment. Values are write-only through the API.
	Keys []string `json:"keys"`
	// Faz 17: every stored row with its environment. Target "all" means
	// both environments (rows written without a target).
	Vars []store.EnvVar `json:"vars"`
}

func (s *Server) envView(w http.ResponseWriter, r *http.Request, appID int64) {
	vars, err := s.Store.ListEnv(r.Context(), appID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	keys := []string{}
	for _, v := range vars {
		if len(keys) == 0 || keys[len(keys)-1] != v.Key { // sorted by key
			keys = append(keys, v.Key)
		}
	}
	sort.Strings(keys)
	writeJSON(w, http.StatusOK, envView{Keys: keys, Vars: vars})
}

func (s *Server) getEnv(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	s.envView(w, r, app.ID)
}

// envScopedRequest is the Faz 17 form of PUT /env.
type envScopedRequest struct {
	// Target: "production", "preview", or "all"/"" for both environments.
	Target string `json:"target"`
	// GitBranch narrows a preview variable to one branch.
	GitBranch string             `json:"git_branch"`
	Vars      map[string]*string `json:"vars"`
}

// parseEnvChanges accepts both bodies of PUT /env:
//
//	{"KEY": "value", "OLD": null}                      both environments
//	{"target": "preview", "git_branch": "feature/x",
//	 "vars": {"KEY": "value", "OLD": null}}             one environment
//
// The second form is recognized by "vars" holding an object (a variable
// named "vars" in the first form holds a string or null).
func parseEnvChanges(r *http.Request) ([]store.EnvChange, error) {
	var raw map[string]json.RawMessage
	if err := decodeJSON(r, &raw); err != nil {
		return nil, err
	}
	var req envScopedRequest
	if v, ok := raw["vars"]; ok && len(v) > 0 && v[0] == '{' {
		for k := range raw {
			if k != "vars" && k != "target" && k != "git_branch" {
				return nil, errors.New("unknown field " + strconv.Quote(k) + " next to \"vars\"")
			}
		}
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &req); err != nil {
			return nil, errors.New("invalid JSON body: " + err.Error())
		}
	} else {
		req.Vars = map[string]*string{}
		for k, v := range raw {
			var val *string
			if err := json.Unmarshal(v, &val); err != nil {
				return nil, errors.New("value of " + strconv.Quote(k) + " must be a string or null")
			}
			req.Vars[k] = val
		}
	}
	if req.Target != "" && !store.ValidEnvTarget(req.Target) {
		return nil, errors.New(`target must be "production", "preview" or "all"`)
	}
	if req.GitBranch != "" && req.Target != store.EnvPreview {
		return nil, errors.New(`git_branch needs target "preview"`)
	}
	if len(req.GitBranch) > 255 {
		return nil, errors.New("git_branch is too long")
	}
	keys := make([]string, 0, len(req.Vars))
	for k := range req.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	changes := make([]store.EnvChange, 0, len(keys))
	for _, k := range keys {
		if err := CheckEnvVar(k, req.Vars[k]); err != nil {
			return nil, err
		}
		changes = append(changes, store.EnvChange{Key: k, Value: req.Vars[k], Target: req.Target, GitBranch: req.GitBranch})
	}
	return changes, nil
}

// putEnv merges changes into the app's variables; null deletes. Without a
// target the change applies to both environments. Changes apply to
// deployments created afterwards.
func (s *Server) putEnv(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	changes, err := parseEnvChanges(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.ApplyEnvChanges(r.Context(), app.ID, changes); err != nil {
		s.internalError(w, err)
		return
	}
	target := ""
	if len(changes) > 0 {
		target = changes[0].Target
	}
	s.Log.Info("env updated", "app", app.Name, "changed", len(changes), "target", target)
	s.envView(w, r, app.ID)
}

// githubWebhook turns a signed push delivery into a queued deployment.
func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	if err := webhook.VerifySignature(s.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	switch event := r.Header.Get("X-GitHub-Event"); event {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"result": "pong"})
		return
	case "push":
	case "pull_request":
		s.pullRequestClosed(w, r, body)
		return
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored", "reason": "event " + event})
		return
	}

	push, err := webhook.ParsePush(body)
	if errors.Is(err, webhook.ErrBranchDeleted) {
		s.branchDeleted(w, r, push.Repo, push.Branch, "branch deleted")
		return
	}
	if errors.Is(err, webhook.ErrIgnored) {
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored", "reason": "not a branch push"})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	app, err := s.Store.GetAppByRepo(r.Context(), push.Repo)
	if errors.Is(err, store.ErrNotFound) {
		// 202 rather than 4xx: GitHub marks failed deliveries red, and an
		// unconnected repo is not an error on GitHub's side.
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored", "reason": "no app for repo " + push.Repo})
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	// Faz 17: ignored build step. The commit stays undeployed; the branch
	// keeps its previous deployment.
	if push.SkipMarker != "" {
		s.Log.Info("deployment skipped", "app", app.Name, "branch", push.Branch,
			"sha", naming.ShortSHA(push.SHA), "marker", push.SkipMarker)
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored",
			"reason": "head commit message contains " + push.SkipMarker})
		return
	}

	d, created, err := s.Store.EnqueueDeployment(r.Context(), app.ID, push.SHA, push.Branch, push.Message)
	if err != nil {
		s.internalError(w, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	s.Log.Info("deployment queued", "app", app.Name, "branch", push.Branch,
		"sha", naming.ShortSHA(push.SHA), "deployment", d.ID, "new", created)
	writeJSON(w, status, s.deploymentView(d))
}

// ---- helpers ----

// lookupApp returns the {name} app checked by requireApp; for an unwrapped
// handler it checks the caller's role itself (viewer for reads, member
// otherwise), so a new endpoint is never left open.
func (s *Server) lookupApp(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	if app, ok := r.Context().Value(appCtxKey{}).(store.App); ok {
		return app, true
	}
	return s.authorizeApp(w, r, defaultRole(r.Method))
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.Log.Error("internal error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
