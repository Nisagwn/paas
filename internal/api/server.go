// Package api exposes the minipaas control-plane HTTP API.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/minipaas/internal/naming"
	"github.com/nisagwn/minipaas/internal/store"
	"github.com/nisagwn/minipaas/internal/webhook"
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
	Router        Router
	Domain        string
	APIToken      string
	WebhookSecret string
	Log           *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /webhooks/github", s.githubWebhook)

	api := http.NewServeMux()
	api.HandleFunc("POST /api/apps", s.createApp)
	api.HandleFunc("GET /api/apps", s.listApps)
	api.HandleFunc("GET /api/apps/{name}", s.getApp)
	api.HandleFunc("GET /api/apps/{name}/deployments", s.listDeployments)
	api.HandleFunc("POST /api/apps/{name}/rollback", s.rollback)
	api.HandleFunc("GET /api/apps/{name}/env", s.getEnv)
	api.HandleFunc("PUT /api/apps/{name}/env", s.putEnv)
	api.HandleFunc("GET /api/deployments/{id}", s.getDeployment)
	api.HandleFunc("GET /api/deployments/{id}/logs", s.deploymentLogs)
	mux.Handle("/api/", s.requireToken(api))

	return s.logRequests(mux)
}

// ---- middleware ----

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.APIToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

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
	app, err := s.Store.CreateApp(r.Context(), req.Name, req.Repo, req.ProductionBranch)
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
	apps, err := s.Store.ListApps(r.Context())
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
	return appView{App: a, ProductionURL: "https://" + naming.ProductionHost(a.Name, s.Domain), Aliases: aliases}
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
}

func (s *Server) deploymentView(d store.Deployment) deploymentView {
	return deploymentView{Deployment: d, URL: "https://" + naming.DeploymentHost(d.CommitSHA, d.AppName, s.Domain)}
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
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	d, err := s.Store.GetDeployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.deploymentView(d))
}

// deploymentLogs returns lines after ?after=<id>. Faz 5 adds an SSE stream.
func (s *Server) deploymentLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = n
	}
	if _, err := s.Store.GetDeployment(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
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
	// Keys only: values are write-only through the API.
	Keys []string `json:"keys"`
}

func (s *Server) envView(w http.ResponseWriter, r *http.Request, appID int64) {
	env, err := s.Store.AppEnv(r.Context(), appID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	writeJSON(w, http.StatusOK, envView{Keys: keys})
}

func (s *Server) getEnv(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	s.envView(w, r, app.ID)
}

// putEnv merges {"KEY": "value", "OLD": null} into the app's variables;
// null deletes. Changes apply to deployments created afterwards.
func (s *Server) putEnv(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var changes map[string]*string
	if err := decodeJSON(r, &changes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for k, v := range changes {
		switch {
		case !envKeyRe.MatchString(k):
			writeError(w, http.StatusBadRequest, "invalid variable name "+strconv.Quote(k))
			return
		case k == "PORT" || strings.HasPrefix(k, "MINIPAAS_"):
			writeError(w, http.StatusBadRequest, k+" is set by the platform")
			return
		case v != nil && len(*v) > maxEnvValue:
			writeError(w, http.StatusBadRequest, k+" is longer than 32 KiB")
			return
		}
	}
	if err := s.Store.UpdateAppEnv(r.Context(), app.ID, changes); err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("env updated", "app", app.Name, "changed", len(changes))
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
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored", "reason": "event " + event})
		return
	}

	push, err := webhook.ParsePush(body)
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

func (s *Server) lookupApp(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	app, err := s.Store.GetAppByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "app not found")
		return app, false
	}
	if err != nil {
		s.internalError(w, err)
		return app, false
	}
	return app, true
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
