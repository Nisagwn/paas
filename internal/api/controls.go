package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 17: deploy controls (Vercel parity).
//
//	POST /api/apps/{name}/promote                    member
//	POST /api/apps/{name}/deployments/{id}/redeploy  member
//	POST /api/deployments/{id}/cancel                member of the deployment's app
//	GET|POST /api/apps/{name}/hooks                  member (hooks are secrets)
//	DELETE /api/apps/{name}/hooks/{id}               member
//	POST /api/hooks/deploy/{token}                   the token only
//
// Promote and redeploy never rebuild what is already built: they queue a new
// deployment of the same commit that reuses the source's pinned image, and
// the worker skips the build. A new deployment (rather than re-pointing the
// alias at the source) is needed because variables differ per environment
// and are snapshotted into each deployment's own Secret: the promoted copy
// runs the same image with the production variables, while the preview
// keeps running with the preview ones.

type promoteRequest struct {
	DeploymentID int64 `json:"deployment_id"`
}

// promoteView is the answer of POST /promote. Mode "alias": the source
// already runs with production variables, so the production alias moved to
// it at once (a rollback). Mode "deployment": a production copy of the
// preview deployment was queued; production moves when it is ready.
type promoteView struct {
	Mode       string          `json:"mode"`
	Alias      *store.Alias    `json:"alias,omitempty"`
	Deployment *deploymentView `json:"deployment,omitempty"`
}

// appDeployment loads deployment id if it belongs to app (404 otherwise).
func (s *Server) appDeployment(w http.ResponseWriter, r *http.Request, app store.App, id int64) (store.Deployment, bool) {
	d, err := s.Store.GetDeployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.AppID != app.ID) {
		writeError(w, http.StatusNotFound, "deployment not found for this app")
		return d, false
	}
	if err != nil {
		s.internalError(w, err)
		return d, false
	}
	return d, true
}

func (s *Server) promote(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var req promoteRequest
	if err := decodeJSON(r, &req); err != nil || req.DeploymentID <= 0 {
		writeError(w, http.StatusBadRequest, "body must be {\"deployment_id\": <id>}")
		return
	}
	src, ok := s.appDeployment(w, r, app, req.DeploymentID)
	if !ok {
		return
	}
	res, err := Promote(r.Context(), s.Store, src, principal(r).Login)
	var conflict *ConflictError
	switch {
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "production alias not found for this app")
		return
	case errors.Is(err, store.ErrRetired), errors.Is(err, store.ErrNotReady):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, err)
		return
	}

	if res.Alias != nil {
		alias := *res.Alias
		s.Log.Info("promote", "app", app.Name, "deployment", src.ID, "mode", "alias")
		if s.Router != nil {
			if err := s.Router.SyncApp(r.Context(), app.Name); err != nil {
				s.Log.Error("promote: route sync", "app", app.Name, "err", err)
				writeError(w, http.StatusBadGateway,
					"promotion saved, but updating the router failed (retried automatically): "+err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, promoteView{Mode: "alias", Alias: &alias})
		return
	}

	d := *res.Deployment
	s.Log.Info("promote", "app", app.Name, "source", src.ID, "deployment", d.ID, "mode", "deployment",
		"reuses_image", d.Image != "")
	v := s.deploymentView(d)
	writeJSON(w, http.StatusAccepted, promoteView{Mode: "deployment", Deployment: &v})
}

// ConflictError is a refusal of Promote, Redeploy or Cancel because of the
// deployment's state (409); the text is meant for the user.
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// PromoteResult: Alias is set when the production alias moved at once (the
// source already runs in production), Deployment when a production copy
// was queued.
type PromoteResult struct {
	Alias      *store.Alias
	Deployment *store.Deployment
}

// Promote takes ready deployment src to production without a build (see
// the comment at the top). by is who asked, for the build log. The caller
// syncs the router when the alias moved. Errors: *ConflictError, and
// store.ErrNotFound / ErrRetired / ErrNotReady of the alias move.
func Promote(ctx context.Context, st *store.Store, src store.Deployment, by string) (PromoteResult, error) {
	switch src.Status {
	case store.StatusReady:
	case store.StatusRetired:
		return PromoteResult{}, &ConflictError{"deployment is retired: redeploy it first"}
	default:
		return PromoteResult{}, &ConflictError{"only ready deployments can be promoted (status " + src.Status + ")"}
	}
	if src.Target == store.EnvProduction {
		// Already built and running with production variables: move the
		// alias, exactly like a rollback.
		alias, err := st.Rollback(ctx, src.AppID, src.ID)
		if err != nil {
			return PromoteResult{}, err
		}
		return PromoteResult{Alias: &alias}, nil
	}
	d, err := st.CopyDeployment(ctx, src, store.CopyOptions{
		Origin: store.OriginPromote, Target: store.EnvProduction, ReuseImage: true,
	})
	if err != nil {
		return PromoteResult{}, err
	}
	st.AppendLog(ctx, d.ID, "==> promotion of deployment #"+strconv.FormatInt(src.ID, 10)+" to production by "+by)
	return PromoteResult{Deployment: &d}, nil
}

// Redeploy queues a new deployment of finished deployment src in its
// environment; useCache reuses its image. Error: *ConflictError while src
// is still in progress.
func Redeploy(ctx context.Context, st *store.Store, src store.Deployment, useCache bool, by string) (store.Deployment, error) {
	target := src.Target
	if target == "" {
		target = store.EnvPreview
	}
	d, err := st.CopyDeployment(ctx, src, store.CopyOptions{
		Origin: store.OriginRedeploy, Target: target, ReuseImage: useCache,
	})
	if errors.Is(err, store.ErrInFlight) {
		return d, &ConflictError{"deployment is still " + src.Status + "; cancel it or wait until it finishes"}
	}
	if err != nil {
		return d, err
	}
	how := "rebuild"
	if d.Image != "" {
		how = "reusing image " + d.Image
	}
	st.AppendLog(ctx, d.ID, "==> redeploy of deployment #"+strconv.FormatInt(src.ID, 10)+" by "+by+" ("+how+")")
	return d, nil
}

// Cancel cancels deployment d: a queued one at once (the result is
// "canceled"), a running one at the worker's next heartbeat. Errors:
// store.ErrNotFound, *ConflictError when it already finished.
func Cancel(ctx context.Context, st *store.Store, d store.Deployment, by string) (store.Deployment, error) {
	cur, err := st.CancelDeployment(ctx, d.ID)
	if errors.Is(err, store.ErrFinished) {
		return d, &ConflictError{"deployment already finished (" + d.Status + ")"}
	}
	if err != nil {
		return d, err
	}
	if cur.Status == store.StatusCanceled {
		st.AppendLog(ctx, cur.ID, "==> canceled by "+by+" before it started")
	} else {
		// The worker stops at its next heartbeat and records "canceled".
		st.AppendLog(ctx, cur.ID, "==> cancel requested by "+by)
	}
	return cur, nil
}

type redeployRequest struct {
	// UseCache false forces a rebuild; default true reuses the image.
	UseCache *bool `json:"use_cache"`
}

func (s *Server) redeploy(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req redeployRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	src, ok := s.appDeployment(w, r, app, id)
	if !ok {
		return
	}
	reuse := req.UseCache == nil || *req.UseCache
	d, err := Redeploy(r.Context(), s.Store, src, reuse, principal(r).Login)
	var conflict *ConflictError
	if errors.As(err, &conflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("redeploy", "app", app.Name, "source", src.ID, "deployment", d.ID, "reuses_image", d.Image != "")
	writeJSON(w, http.StatusAccepted, s.deploymentView(d))
}

func (s *Server) cancelDeployment(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDeploymentAs(w, r, store.RoleMember)
	if !ok {
		return
	}
	cur, err := Cancel(r.Context(), s.Store, d, principal(r).Login)
	var conflict *ConflictError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	d = cur
	s.Log.Info("deployment cancel", "app", d.AppName, "deployment", d.ID, "status", d.Status, "by", principal(r).Login)
	if d.Status == store.StatusCanceled {
		writeJSON(w, http.StatusOK, s.deploymentView(d))
		return
	}
	writeJSON(w, http.StatusAccepted, s.deploymentView(d))
}

// ---- deploy hooks ----

// HookTokenPrefix starts every deploy hook token.
const HookTokenPrefix = "paas_hook_"

// HookPath is where a deploy hook token is posted.
func HookPath(token string) string { return "/api/hooks/deploy/" + token }

type createHookRequest struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
}

// createdHookView carries the plain token once; it is never shown again.
type createdHookView struct {
	store.DeployHook
	Token string `json:"token"`
	Path  string `json:"path"`
}

func (s *Server) listHooks(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	hooks, err := s.Store.ListDeployHooks(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hooks)
}

func (s *Server) createHook(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var req createHookRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name, branch, err := NormalizeHook(app, req.Name, req.Branch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h, plain, err := CreateHook(r.Context(), s.Store, app, name, branch, principal(r).UserID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("deploy hook created", "app", app.Name, "hook", h.ID, "branch", h.Branch, "by", principal(r).Login)
	writeJSON(w, http.StatusCreated, createdHookView{DeployHook: h, Token: plain, Path: HookPath(plain)})
}

// NormalizeHook validates a new hook's name and branch: the branch defaults
// to the production branch, the name to the branch.
func NormalizeHook(app store.App, name, branch string) (string, string, error) {
	name, branch = strings.TrimSpace(name), strings.TrimSpace(branch)
	if branch == "" {
		branch = app.ProductionBranch
	}
	if name == "" {
		name = branch
	}
	if len(name) > 100 || len(branch) > 255 || strings.ContainsAny(branch, " \t\n~^:?*[\\") {
		return name, branch, errors.New("name must be at most 100 characters and branch a valid branch name")
	}
	return name, branch, nil
}

// CreateHook stores a new hook with a fresh token and returns the token,
// which is never available again. createdBy 0 is the admin token.
func CreateHook(ctx context.Context, st *store.Store, app store.App, name, branch string, createdBy int64) (store.DeployHook, string, error) {
	plain, hash, prefix := NewHookToken()
	h, err := st.CreateDeployHook(ctx, app.ID, name, branch, hash, prefix, createdBy)
	return h, plain, err
}

func (s *Server) deleteHook(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.Store.DeleteDeployHook(r.Context(), app.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "hook not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("deploy hook deleted", "app", app.Name, "hook", id, "by", principal(r).Login)
	w.WriteHeader(http.StatusNoContent)
}

// NewHookToken returns a fresh hook token (256 random bits), its SHA-256
// (stored) and the prefix shown in listings.
func NewHookToken() (plain, hash, prefix string) {
	b := make([]byte, 32)
	rand.Read(b)
	plain = HookTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return plain, auth.HashToken(plain), plain[:len(HookTokenPrefix)+6]
}

// triggerHook deploys the latest commit of the hook's branch. Like Vercel's
// deploy hooks it always produces a deployment: a new commit is queued as
// usual; if the head commit was deployed already, it is rebuilt (hooks are
// typically fired when content outside the repository changed), unless that
// deployment is still in progress, which is returned instead.
func (s *Server) triggerHook(w http.ResponseWriter, r *http.Request) {
	if s.GitHubApp == nil {
		writeError(w, http.StatusNotImplemented, "deploy hooks need the GitHub App to read branch heads")
		return
	}
	token := r.PathValue("token")
	if !strings.HasPrefix(token, HookTokenPrefix) {
		writeError(w, http.StatusNotFound, "unknown deploy hook")
		return
	}
	hook, err := s.Store.TriggerDeployHook(r.Context(), auth.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown deploy hook")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	app, err := s.Store.GetAppByID(r.Context(), hook.AppID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	cctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	sha, err := s.GitHubApp.BranchHead(cctx, app.Repo, hook.Branch)
	cancel()
	if err != nil {
		s.Log.Warn("deploy hook: branch head", "app", app.Name, "branch", hook.Branch, "err", err)
		writeError(w, http.StatusBadGateway, "reading the head of "+hook.Branch+" failed: "+err.Error())
		return
	}
	message := "Deploy hook: " + hook.Name
	d, created, err := s.Store.EnqueueDeploymentFrom(r.Context(), app.ID, sha, hook.Branch, message, store.OriginHook)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !created && d.Finished() {
		target := store.EnvPreview
		if hook.Branch == app.ProductionBranch {
			target = store.EnvProduction
		}
		// The deployment of the head may be another branch's (same commit):
		// the copy takes the hook's branch.
		d.Branch = hook.Branch
		d, err = s.Store.CopyDeployment(r.Context(), d, store.CopyOptions{
			Origin: store.OriginHook, Target: target, Message: message,
		})
		if err != nil {
			s.internalError(w, err)
			return
		}
		created = true
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	s.Log.Info("deploy hook", "app", app.Name, "hook", hook.ID, "branch", hook.Branch,
		"sha", naming.ShortSHA(sha), "deployment", d.ID, "new", created)
	writeJSON(w, status, s.deploymentView(d))
}

// decodeOptionalJSON is decodeJSON for bodies that may be empty.
func decodeOptionalJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}
