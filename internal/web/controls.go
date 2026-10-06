package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/build"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 16–19 screens: deploy controls (promote, redeploy, cancel), build settings and
// deploy hooks in the UI. They call the same functions as the JSON API
// (internal/api controls.go, settings.go), so both behave alike. Every form
// is a member+ POST with the session's CSRF token (authed, loadApp).

// formDeployment loads the form's deployment_id if it belongs to app; on
// failure it renders the deployments tab and returns false.
func (s *Server) formDeployment(w http.ResponseWriter, r *http.Request, app store.App) (store.Deployment, bool) {
	id, err := strconv.ParseInt(r.PostFormValue("deployment_id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderTabOf(w, r, app, http.StatusBadRequest, tabDeploys, "Geçersiz deploy.", nil)
		return store.Deployment{}, false
	}
	d, err := s.Store.GetDeployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.AppID != app.ID) {
		s.renderTabOf(w, r, app, http.StatusNotFound, tabDeploys, "Deploy bulunamadı.", nil)
		return d, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return d, false
	}
	return d, true
}

// who names the caller in build logs.
func who(r *http.Request) string {
	p, _ := auth.From(r.Context())
	return p.Login
}

// controlError renders a refusal of api.Promote / Redeploy / Cancel; it
// returns false when err is nil.
func (s *Server) controlError(w http.ResponseWriter, r *http.Request, app store.App, err error) bool {
	var conflict *api.ConflictError
	switch {
	case err == nil:
		return false
	case errors.As(err, &conflict):
		s.renderTabOf(w, r, app, http.StatusConflict, tabDeploys, err.Error(), nil)
	case errors.Is(err, store.ErrNotFound):
		s.renderTabOf(w, r, app, http.StatusNotFound, tabDeploys, "Bu projede böyle bir deploy ya da canlı adres yok.", nil)
	case errors.Is(err, store.ErrRetired), errors.Is(err, store.ErrNotReady):
		s.renderTabOf(w, r, app, http.StatusConflict, tabDeploys, "Yalnızca hazır bir deploy canlıya alınabilir.", nil)
	default:
		s.internalError(w, r, err)
	}
	return true
}

func (s *Server) promote(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	src, ok := s.formDeployment(w, r, app)
	if !ok {
		return
	}
	res, err := api.Promote(r.Context(), s.Store, src, who(r))
	if s.controlError(w, r, app, err) {
		return
	}
	if res.Alias != nil {
		s.Log.Info("promote", "app", app.Name, "deployment", src.ID, "mode", "alias", "via", "web")
		if !s.syncRoutes(w, r, app, "Production'a taşıma") {
			return
		}
		redirect(w, r, "/apps/"+app.Name+"/deployments?ok=promoted-alias")
		return
	}
	s.Log.Info("promote", "app", app.Name, "source", src.ID, "deployment", res.Deployment.ID, "mode", "deployment", "via", "web")
	redirect(w, r, "/apps/"+app.Name+"/deployments?ok=promoted")
}

func (s *Server) redeploy(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	src, ok := s.formDeployment(w, r, app)
	if !ok {
		return
	}
	useCache := r.PostFormValue("no_cache") == ""
	d, err := api.Redeploy(r.Context(), s.Store, src, useCache, who(r))
	if s.controlError(w, r, app, err) {
		return
	}
	s.Log.Info("redeploy", "app", app.Name, "source", src.ID, "deployment", d.ID, "reuses_image", d.Image != "", "via", "web")
	redirect(w, r, "/apps/"+app.Name+"/deployments?ok=redeploy")
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	d, ok := s.formDeployment(w, r, app)
	if !ok {
		return
	}
	d, err := api.Cancel(r.Context(), s.Store, d, who(r))
	if s.controlError(w, r, app, err) {
		return
	}
	s.Log.Info("deployment cancel", "app", app.Name, "deployment", d.ID, "status", d.Status, "via", "web")
	flash := "cancel"
	if d.Status == store.StatusCanceled {
		flash = "canceled"
	}
	redirect(w, r, "/apps/"+app.Name+"/deployments?ok="+flash)
}

// ---- build settings ----

// saveSettings replaces the build settings with the form; an empty field
// means "detect".
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	form := store.BuildSettings{
		Framework:       r.PostFormValue("framework"),
		RootDirectory:   r.PostFormValue("root_directory"),
		InstallCommand:  r.PostFormValue("install_command"),
		BuildCommand:    r.PostFormValue("build_command"),
		StartCommand:    r.PostFormValue("start_command"),
		OutputDirectory: r.PostFormValue("output_directory"),
		NodeVersion:     r.PostFormValue("node_version"),
	}
	next, err := build.NormalizeSettings(form)
	if err != nil {
		s.renderTabOf(w, r, app, http.StatusBadRequest, tabSettings, err.Error(), func(v *appDetail) {
			v.Settings = form // keep what was typed
		})
		return
	}
	saved, err := s.Store.UpdateBuildSettings(r.Context(), app.ID, next)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("build settings updated", "app", app.Name, "framework", saved.Framework, "root", saved.RootDirectory, "via", "web")
	http.Redirect(w, r, "/apps/"+app.Name+"/settings?ok=settings#build", http.StatusSeeOther)
}

// ---- deploy hooks ----

// baseURL is the control plane's address as the browser reached it.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) createHook(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	name, branch, err := api.NormalizeHook(app, r.PostFormValue("name"), r.PostFormValue("branch"))
	if err != nil {
		s.renderTabOf(w, r, app, http.StatusBadRequest, tabSettings, err.Error(), nil)
		return
	}
	me, _ := auth.From(r.Context())
	h, plain, err := api.CreateHook(r.Context(), s.Store, app, name, branch, me.UserID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("deploy hook created", "app", app.Name, "hook", h.ID, "branch", h.Branch, "by", me.Login, "via", "web")
	// The token is shown on this response only.
	s.renderTabOf(w, r, app, http.StatusCreated, tabSettings, "", func(v *appDetail) {
		v.NewHookURL = baseURL(r) + api.HookPath(plain)
	})
}

func (s *Server) deleteHook(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err == nil {
		err = s.Store.DeleteDeployHook(r.Context(), app.ID, id)
	}
	switch {
	case errors.Is(err, store.ErrNotFound), err != nil && id == 0:
		s.renderTabOf(w, r, app, http.StatusNotFound, tabSettings, "Deploy hook'u bulunamadı.", nil)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("deploy hook deleted", "app", app.Name, "hook", id, "by", who(r), "via", "web")
	http.Redirect(w, r, "/apps/"+app.Name+"/settings?ok=hook-deleted#hooks", http.StatusSeeOther)
}
