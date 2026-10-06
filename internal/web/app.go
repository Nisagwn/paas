package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/build"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// The app page (the Faz 16–19 screens) has four server-rendered tabs:
//
//	/apps/{name}              Genel: addresses, production, recent deployments
//	/apps/{name}/deployments  Deploy'lar: history and deploy controls
//	/apps/{name}/analytics    Analitik (analytics.go)
//	/apps/{name}/settings     Ayarlar: build settings, variables, deploy hooks,
//	                          custom domains, sleep mode
//
// Each tab is its own page template sharing the "apphead" partial.

// Tabs, by page template name.
const (
	tabOverview  = "app"
	tabDeploys   = "deploys"
	tabAnalytics = "analytics"
	tabSettings  = "settings"
)

var tabTitles = map[string]string{
	tabOverview:  "Genel",
	tabDeploys:   "Deploy'lar",
	tabAnalytics: "Analitik",
	tabSettings:  "Ayarlar",
}

// recentDeployments is how many deployments the overview lists.
const recentDeployments = 5

type deploymentRow struct {
	store.Deployment
	URL        string
	Production bool
	// Member+ controls, by state.
	CanRollback bool
	CanPromote  bool
	CanRedeploy bool
	CanCancel   bool
	AliasesHere []string
	// Rollout is the latest canary/guard of this deployment (Faz 21), if any.
	Rollout *store.Rollout
}

type aliasRow struct {
	store.Alias
	URL string
	SHA string
}

// envGroup is the variables of one target (Tümü / Production / Önizleme).
type envGroup struct {
	Target string
	Vars   []store.EnvVar
}

type appDetail struct {
	App           store.App
	Tab           string
	ProductionURL string
	Aliases       []aliasRow
	Deployments   []deploymentRow
	Recent        []deploymentRow
	Production    *deploymentRow
	Active        bool // something is in progress: the table polls
	HasProduction bool
	Domains       []api.DomainView
	// Faz 13: the caller's role on the app's team; CanWrite = member+.
	Team     store.Team
	Role     string
	CanWrite bool

	// Ayarlar. EnvKeys are the distinct variable names.
	EnvKeys    []string
	EnvGroups  []envGroup
	Settings   store.BuildSettings
	Detected   string
	Frameworks []build.Framework
	// Hooks are listed to members only (like the API). NewHookURL is the
	// URL of a hook just created, shown once.
	Hooks      []store.DeployHook
	NewHookURL string
	HooksWork  bool // the GitHub App reads branch heads; without it hooks answer 501

	// Analitik (analytics.go).
	A *analyticsData

	// Faz 21: canary / guarded rollouts (rollouts.go).
	Rollout *rolloutPanel
}

// loadApp loads the {name} app and checks the caller's team role: viewer
// for GET, member for POST. Apps of other teams look like missing ones.
func (s *Server) loadApp(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	app, err := s.Store.GetAppByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Proje bulunamadı.")
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
	if !s.checkTeam(w, r, app.TeamID, need, "Proje bulunamadı.") {
		return app, false
	}
	return app, true
}

func (s *Server) appDetail(ctx context.Context, app store.App) (appDetail, error) {
	v := appDetail{App: app, ProductionURL: naming.URL(s.Scheme, naming.ProductionHost(app.Name, s.Domain)),
		Frameworks: build.Frameworks, HooksWork: s.GitHubApp != nil}
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
	if v.Rollout, err = s.rolloutPanel(ctx, app); err != nil {
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
		ready := d.Status == store.StatusReady
		running := !d.Finished()
		row := deploymentRow{
			Deployment:  d,
			URL:         naming.URL(s.Scheme, d.Host(s.Domain)),
			Production:  d.ID == prodID,
			AliasesHere: hosts[d.ID],
		}
		if r, ok := v.Rollout.ByDeployment[d.ID]; ok {
			row.Rollout = &r
		}
		if v.CanWrite {
			// Rollback moves production to an older production build;
			// a preview is promoted instead (it needs production variables).
			row.CanRollback = v.HasProduction && d.ID != prodID && ready && d.Target != store.EnvPreview
			row.CanPromote = ready && d.Target == store.EnvPreview
			row.CanRedeploy = !running
			row.CanCancel = running && d.CancelRequestedAt == nil
		}
		v.Deployments = append(v.Deployments, row)
		if running {
			v.Active = true
		}
	}
	for i := range v.Deployments {
		if v.Deployments[i].Production {
			v.Production = &v.Deployments[i]
		}
	}
	v.Recent = v.Deployments[:min(len(v.Deployments), recentDeployments)]

	vars, err := s.Store.ListEnv(ctx, app.ID)
	if err != nil {
		return v, err
	}
	v.EnvGroups = []envGroup{{Target: store.EnvAll}, {Target: store.EnvProduction}, {Target: store.EnvPreview}}
	for _, e := range vars {
		if len(v.EnvKeys) == 0 || v.EnvKeys[len(v.EnvKeys)-1] != e.Key { // sorted by key
			v.EnvKeys = append(v.EnvKeys, e.Key)
		}
		for i := range v.EnvGroups {
			if v.EnvGroups[i].Target == e.Target {
				v.EnvGroups[i].Vars = append(v.EnvGroups[i].Vars, e)
			}
		}
	}
	if v.Settings, err = s.Store.GetBuildSettings(ctx, app.ID); err != nil {
		return v, err
	}
	if v.Detected, err = s.Store.DetectedFramework(ctx, app.ID); err != nil {
		return v, err
	}
	if v.CanWrite {
		if v.Hooks, err = s.Store.ListDeployHooks(ctx, app.ID); err != nil {
			return v, err
		}
	}
	v.Domains, err = s.domainViews(ctx, app)
	return v, err
}

// renderTab renders one tab of the app; edit adjusts the data first (form
// values after a validation error, a hook URL shown once).
func (s *Server) renderTab(w http.ResponseWriter, r *http.Request, status int, tab, errMsg string, edit func(*appDetail)) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	s.renderTabOf(w, r, app, status, tab, errMsg, edit)
}

func (s *Server) renderTabOf(w http.ResponseWriter, r *http.Request, app store.App, status int, tab, errMsg string, edit func(*appDetail)) {
	v, err := s.appDetail(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	v.Tab = tab
	if tab == tabAnalytics {
		if v.A, err = s.analytics(r, app); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	if edit != nil {
		edit(&v)
	}
	title := app.Name
	if tab != tabOverview {
		title += " · " + tabTitles[tab]
	}
	s.render(w, r, status, tab, title, v, errMsg)
}

func (s *Server) appPage(w http.ResponseWriter, r *http.Request) {
	s.renderTab(w, r, http.StatusOK, tabOverview, "", nil)
}

// deploymentsPage is the Deploy'lar tab; htmx polls it for the table
// fragment while a deployment is in progress.
func (s *Server) deploymentsPage(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		s.renderTab(w, r, http.StatusOK, tabDeploys, "", nil)
		return
	}
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

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.renderTab(w, r, http.StatusOK, tabSettings, "", nil)
}

// ---- rollback ----

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	fail := func(status int, msg string) { s.renderTabOf(w, r, app, status, tabDeploys, msg, nil) }
	id, err := strconv.ParseInt(r.PostFormValue("deployment_id"), 10, 64)
	if err != nil || id <= 0 {
		fail(http.StatusBadRequest, "Geçersiz deploy.")
		return
	}
	_, err = s.Store.Rollback(r.Context(), app.ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(http.StatusNotFound, "Bu projede böyle bir deploy ya da canlı adres yok.")
		return
	case errors.Is(err, store.ErrNotReady):
		fail(http.StatusConflict, "Yalnızca hazır bir deploy canlıya alınabilir.")
		return
	case errors.Is(err, store.ErrRetired):
		fail(http.StatusConflict,
			"Bu deploy emekliye ayrıldı ve artık çalışmıyor; yeniden yayınlamak için \"Yeniden deploy et\"i kullan.")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("rollback", "app", app.Name, "deployment", id, "via", "web")
	if !s.syncRoutes(w, r, app, "Geri alma") {
		return
	}
	redirect(w, r, "/apps/"+app.Name+"/deployments?ok=rollback")
}

// syncRoutes applies a production alias change to the router; on failure
// it renders the deployments tab and returns false.
func (s *Server) syncRoutes(w http.ResponseWriter, r *http.Request, app store.App, what string) bool {
	if s.Router == nil {
		return true
	}
	if err := s.Router.SyncApp(r.Context(), app.Name); err != nil {
		s.Log.Error("web: route sync", "app", app.Name, "err", err)
		s.renderTabOf(w, r, app, http.StatusBadGateway, tabDeploys,
			what+" kaydedildi ama yönlendirme güncellenemedi; otomatik olarak tekrar denenecek.", nil)
		return false
	}
	return true
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

// changeEnv sets or deletes one variable in the target of the form ("" or
// "all": both environments; a preview variable may name a branch).
func (s *Server) changeEnv(w http.ResponseWriter, r *http.Request, key string, value *string) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	target := strings.TrimSpace(r.PostFormValue("target"))
	branch := strings.TrimSpace(r.PostFormValue("git_branch"))
	if target != store.EnvPreview && value != nil {
		branch = "" // the branch field only applies to previews
	}
	status, msg := http.StatusOK, ""
	if err := api.CheckEnvScope(target, branch); err != nil {
		status, msg = http.StatusBadRequest, err.Error()
	} else if err := api.CheckEnvVar(key, value); err != nil {
		status, msg = http.StatusBadRequest, err.Error()
	} else if err := s.Store.ApplyEnvChanges(r.Context(), app.ID,
		[]store.EnvChange{{Key: key, Value: value, Target: target, GitBranch: branch}}); err != nil {
		s.internalError(w, r, err)
		return
	} else {
		s.Log.Info("env updated", "app", app.Name, "changed", 1, "target", target, "via", "web")
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
		s.renderTabOf(w, r, app, status, tabSettings, msg, nil)
		return
	}
	http.Redirect(w, r, "/apps/"+app.Name+"/settings?ok=env#env", http.StatusSeeOther)
}
