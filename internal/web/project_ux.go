package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// View models of the project and deployment screens: the production hero
// and entry points of Genel, the deploy list filters, the step timeline,
// failure explanation and action bar of a deployment, and the bulk .env
// editor of Ayarlar. Strings live in tr_ux_project.go.

func init() {
	// failtitle: the plain-Turkish headline of a failed deployment's error.
	funcs["failtitle"] = func(d store.Deployment) string { return failureTexts[failureKind(d)][0] }
}

func (s *Server) projectUXRoutes(mux *http.ServeMux) {
	mux.Handle("POST /apps/{name}/env/bulk", s.authed(s.setEnvBulk))
}

type filterOption struct {
	ID, Label string
}

// ---- Genel ----

type overviewUX struct {
	// Health of the production deployment in the last 15 minutes
	// (api.Health); empty when unknown.
	Health     string
	HealthReqs int64
	// Rollback is the previous production deployment "Geri al" returns to;
	// RollbackReason says why the button is disabled when it is nil.
	Rollback       *deploymentRow
	RollbackReason string
	// RedeployReason disables "Yeniden deploy et" of the hero.
	RedeployReason string
	// Previews are the branch aliases, without the production branch.
	Previews []aliasRow
	// Attention is a newer deployment than production that failed or is
	// still running.
	Attention   *deploymentRow
	AddonCount  int
	LiveDomains []api.DomainView
}

func (s *Server) overviewUX(ctx context.Context, v *appDetail) (*overviewUX, error) {
	o := &overviewUX{}
	for _, a := range v.Aliases {
		if a.Kind == store.AliasPreview && a.Branch != v.App.ProductionBranch {
			o.Previews = append(o.Previews, a)
		}
	}
	for _, d := range v.Domains {
		if d.Status == "active" {
			o.LiveDomains = append(o.LiveDomains, d)
		}
	}
	addons, err := s.Store.ListAddons(ctx, v.App.ID)
	if err != nil {
		return nil, err
	}
	o.AddonCount = len(addons)
	seenProd := false
	for i := range v.Deployments {
		d := &v.Deployments[i]
		if d.Production {
			seenProd = true
			continue
		}
		if !seenProd && o.Attention == nil && (d.Status == store.StatusFailed || !d.Finished()) {
			o.Attention = d
		}
		if seenProd && o.Rollback == nil && d.CanRollback {
			o.Rollback = d
		}
	}
	switch {
	case !v.CanWrite:
		o.RollbackReason, o.RedeployReason = reasonRole, reasonRole
	case v.Production == nil:
		o.RollbackReason, o.RedeployReason = reasonNoProd, reasonNoProd
	case o.Rollback == nil:
		o.RollbackReason = reasonNoPrevious
	}
	if v.Production != nil {
		h, err := api.Health(ctx, s.Store, v.App.ID, s.now())
		if err != nil {
			return nil, err
		}
		o.Health = "no_traffic"
		for _, d := range h.Deployments {
			if d.DeploymentID == v.Production.ID {
				o.Health, o.HealthReqs = d.Health, d.Requests
			}
		}
	}
	return o, nil
}

// ---- Deploy'lar: filters ----

type deployFilter struct {
	Env, Branch, Status string
	Envs, Statuses      []filterOption
	Branches            []string
	Rows                []deploymentRow
	Total               int
	// Query is the filter as a query string, kept by the polling fragment.
	Query string
	Any   bool
}

func statusGroup(status string) string {
	switch status {
	case store.StatusQueued, store.StatusBuilding, store.StatusDeploying:
		return "active"
	}
	return status
}

func deployFilterOf(r *http.Request, rows []deploymentRow) *deployFilter {
	q := r.URL.Query()
	f := &deployFilter{Env: q.Get("env"), Branch: q.Get("branch"), Status: q.Get("status"),
		Envs: deployEnvFilters, Statuses: deployStatusFilters, Total: len(rows)}
	branches := map[string]bool{}
	for _, d := range rows {
		branches[d.Branch] = true
	}
	for b := range branches {
		f.Branches = append(f.Branches, b)
	}
	sort.Strings(f.Branches)
	if !validOption(deployEnvFilters, f.Env) {
		f.Env = ""
	}
	if !validOption(deployStatusFilters, f.Status) {
		f.Status = ""
	}
	if !branches[f.Branch] {
		f.Branch = ""
	}
	keep := url.Values{}
	for k, v := range map[string]string{"env": f.Env, "branch": f.Branch, "status": f.Status} {
		if v != "" {
			keep.Set(k, v)
		}
	}
	f.Query, f.Any = keep.Encode(), len(keep) > 0
	f.Rows = []deploymentRow{}
	for _, d := range rows {
		if (f.Env == "" || d.Target == f.Env) && (f.Branch == "" || d.Branch == f.Branch) &&
			(f.Status == "" || statusGroup(d.Status) == f.Status) {
			f.Rows = append(f.Rows, d)
		}
	}
	return f
}

func validOption(opts []filterOption, id string) bool {
	for _, o := range opts {
		if o.ID == id {
			return true
		}
	}
	return false
}

// tabExtras loads what only one tab needs.
func (s *Server) tabExtras(r *http.Request, tab string, v *appDetail) error {
	var err error
	switch tab {
	case tabOverview:
		v.O, err = s.overviewUX(r.Context(), v)
	case tabDeploys:
		v.F = deployFilterOf(r, v.Deployments)
	}
	return err
}

// ---- deployment page ----

// errLine marks build log lines that are errors (the same rule as the
// page's script).
var errLine = regexp.MustCompile(`(?i)^(error|fatal)\b|\berror:|npm err!|exit (code|status):? [1-9]|^#\d+ error`)

type timelineStep struct {
	Key, Label, State, StateLabel string
	Duration, Note                string
}

type uxAction struct {
	Label string
	// Href is a link action, Post a form action.
	Href, Post string
	External   bool
	Confirm    string
	Class      string
	// Reason disables the action and says why.
	Reason  string
	NoCache bool
}

type failureHint struct {
	Kind, Title, Cause, Next string
	Raw                      string
	Links                    []uxLink
}

type uxLink struct{ Label, URL string }

type deploymentUX struct {
	Steps     []timelineStep
	Failure   *failureHint
	Actions   []uxAction
	CanWrite  bool
	Live      bool // the production alias points here
	Active    bool
	Reused    bool // the image of an earlier build was reused
	FirstErr  string
	Source    *int64
	TotalTime string
}

func (s *Server) deploymentUX(r *http.Request, app store.App, d store.Deployment, terminalOn bool) *deploymentUX {
	ctx := r.Context()
	x := &deploymentUX{Active: !d.Finished(), Source: d.SourceDeploymentID}
	role, err := s.Auth.TeamRole(ctx, me(r), app.TeamID)
	x.CanWrite = err == nil && store.RoleAllows(role, store.RoleMember)
	prod, err := s.Store.ProductionAliasTarget(ctx, app.ID)
	hasProd := err == nil
	x.Live = hasProd && prod.ID == d.ID

	lines, err := s.Store.Logs(ctx, d.ID, 0, 20000)
	if err != nil {
		s.Log.Warn("web: deployment logs", "deployment", d.ID, "err", err)
	}
	var deployAt *time.Time
	for i := range lines {
		l := lines[i].Line
		switch {
		case deployAt == nil && (strings.HasPrefix(l, "==> deploying ") || strings.HasPrefix(l, "[dry-run] apply ")):
			deployAt = &lines[i].TS
		case strings.HasPrefix(l, "==> reusing image"):
			x.Reused = true
		}
		if x.FirstErr == "" && errLine.MatchString(l) {
			x.FirstErr = l
		}
	}
	x.Failure = explainFailure(app, d, s.RuntimeLogs)
	x.Steps = timeline(d, deployAt, x.Reused, x.Failure)
	if d.FinishedAt != nil {
		x.TotalTime = shortDuration(d.FinishedAt.Sub(d.CreatedAt))
	}
	x.Actions = s.deploymentActions(app, d, x, hasProd, terminalOn)
	return x
}

func dur(from time.Time, to *time.Time) string {
	if to == nil {
		return ""
	}
	return shortDuration(to.Sub(from))
}

// timeline is Sıraya alındı → Build → Deploy → result.
func timeline(d store.Deployment, deployAt *time.Time, reused bool, f *failureHint) []timelineStep {
	q := timelineStep{Key: stepQueued, Label: stepLabels[stepQueued]}
	b := timelineStep{Key: stepBuild, Label: stepLabels[stepBuild]}
	dp := timelineStep{Key: stepDeploy, Label: stepLabels[stepDeploy]}
	res := timelineStep{Key: stepResult, Label: statusLabel(store.StatusReady), State: stPending}
	if reused {
		b.Note = "önceki imaj kullanıldı"
	}
	if d.StartedAt != nil {
		q.Duration = dur(d.CreatedAt, d.StartedAt)
		end := deployAt
		if end == nil && d.Status != store.StatusBuilding && d.Status != store.StatusQueued {
			end = d.FinishedAt
		}
		b.Duration = dur(*d.StartedAt, end)
		if deployAt != nil {
			dp.Duration = dur(*deployAt, d.FinishedAt)
		}
	}
	// failedAt: the step that was running when the deployment stopped.
	// The explanation decides when it names a phase; otherwise the log does.
	failedAt := stepBuild
	kind := ""
	if f != nil {
		kind = f.Kind
	}
	switch {
	case d.StartedAt == nil:
		failedAt = stepQueued
	case kind == "build" || kind == "fetch" || kind == "root" || kind == "settings" || kind == "processes":
		failedAt = stepBuild
	case kind == "start" || kind == "quota" || deployAt != nil:
		failedAt = stepDeploy
	}
	order := []*timelineStep{&q, &b, &dp}
	set := func(cur string, curState string) {
		state := stDone
		for _, st := range order {
			if st.Key == cur {
				st.State = curState
				state = stPending
				continue
			}
			st.State = state
		}
	}
	switch d.Status {
	case store.StatusQueued:
		set(stepQueued, stCurrent)
	case store.StatusBuilding:
		set(stepBuild, stCurrent)
	case store.StatusDeploying:
		set(stepDeploy, stCurrent)
	case store.StatusFailed:
		set(failedAt, stFailed)
		res.Label, res.State = statusLabel(store.StatusFailed), stFailed
		if failedAt == stepBuild {
			b.Duration = dur(*d.StartedAt, d.FinishedAt)
			dp.Duration = ""
		}
	case store.StatusCanceled:
		set(failedAt, stSkipped)
		res.Label, res.State = statusLabel(store.StatusCanceled), stSkipped
	default: // ready, retired
		set("", stDone)
		res.State = stDone
		if d.Status == store.StatusRetired {
			res.Label = statusLabel(store.StatusRetired)
		}
	}
	for _, st := range order {
		if st.State == stPending || st.State == stSkipped {
			st.Duration = ""
		}
	}
	if d.FinishedAt != nil {
		res.Duration = "toplam " + shortDuration(d.FinishedAt.Sub(d.CreatedAt))
	}
	out := []timelineStep{q, b, dp, res}
	for i := range out {
		out[i].StateLabel = stepStateLabels[out[i].State]
	}
	return out
}

// explainFailure turns the stored error into a plain-Turkish explanation;
// nil when the deployment did not fail or get canceled.
func explainFailure(app store.App, d store.Deployment, runtimeLogs bool) *failureHint {
	if d.Status != store.StatusFailed && d.Status != store.StatusCanceled {
		return nil
	}
	kind := failureKind(d)
	t := failureTexts[kind]
	h := &failureHint{Kind: kind, Title: t[0], Cause: t[1], Next: failureNext[kind], Raw: tr(d.Error)}
	settings := "/apps/" + app.Name + "/settings"
	switch kind {
	case "build", "root", "settings":
		h.Links = []uxLink{{"Ayarlar › Build ayarları", settings + "#build"}}
	case "start":
		h.Links = []uxLink{{"Ayarlar › Build ayarları", settings + "#build"}, {"Ortam değişkenleri", settings + "#env"}}
		if runtimeLogs {
			h.Links = append(h.Links, uxLink{"Çalışma log'u", "#runtime"})
		}
	case "fetch":
		h.Links = []uxLink{{"Ayarlar › Genel", settings + "#general"}}
	case "timeout", "quota":
		h.Links = []uxLink{{"Kullanım ve plan", "/usage"}}
	case "database":
		h.Links = []uxLink{{"Veritabanları", "/apps/" + app.Name + "/databases"}}
	case "processes":
		h.Links = []uxLink{{"Ayarlar › Süreçler", settings + "#processes"}}
	}
	return h
}

func failureKind(d store.Deployment) string {
	if d.Status == store.StatusCanceled {
		return "canceled"
	}
	e := strings.ToLower(d.Error)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(e, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("deadline exceeded", "timeout", "timed out"):
		return "timeout"
	case strings.HasPrefix(e, "databases:"), has("database"):
		return "database"
	case has("kubernetes api unreachable", "kubernetes config", "registry is not set", "not found in path"):
		return "platform"
	case has("build settings"):
		return "settings"
	case has("root directory"):
		return "root"
	case has("fetch source", "git fetch", "git clone", "git checkout", "git rev-parse", "checked out"):
		return "fetch"
	case has("cannot create pods", "quota", "exceeded quota"):
		return "quota"
	case has("rollout stalled", "not ready after", "crash", "readiness", "imagepullbackoff"), strings.HasPrefix(e, "deploy:"):
		return "start"
	case has("process"):
		return "processes"
	case strings.HasPrefix(e, "build:"), has("buildctl", "docker buildx"):
		return "build"
	}
	return "other"
}

func (s *Server) deploymentActions(app store.App, d store.Deployment, x *deploymentUX, hasProd, terminalOn bool) []uxAction {
	base := "/apps/" + app.Name
	ready := d.Status == store.StatusReady && d.RetiredAt == nil
	running := !d.Finished()
	sha := naming.ShortSHA(d.CommitSHA)
	var out []uxAction

	open := uxAction{Label: "Siteyi aç ↗", Href: s.deployURL(d), External: true, Class: "primary"}
	if !ready {
		open.Reason = notReadyReason(d)
	}
	out = append(out, open)

	if d.Target == store.EnvPreview {
		a := uxAction{Label: "Production'a taşı", Post: base + "/promote",
			Confirm: sha + " production'a taşınsın mı? Aynı imaj production değişkenleriyle yeniden yayınlanır."}
		switch {
		case !x.CanWrite:
			a.Reason = reasonRole
		case running:
			a.Reason = reasonRunning
		case !ready:
			a.Reason = notReadyReason(d)
		}
		out = append(out, a)
	} else {
		a := uxAction{Label: "Bu sürüme dön", Post: base + "/rollback", Confirm: "Canlı site " + sha + " sürümüne dönsün mü?"}
		switch {
		case !x.CanWrite:
			a.Reason = reasonRole
		case x.Live:
			a.Reason = reasonIsLive
		case d.Status == store.StatusRetired || d.RetiredAt != nil:
			a.Reason = reasonRetired
		case !ready:
			a.Reason = notReadyReason(d)
		case !hasProd:
			a.Reason = reasonNoProd
		}
		out = append(out, a)
	}

	re := uxAction{Label: "Yeniden deploy et", Post: base + "/redeploy", NoCache: true}
	switch {
	case !x.CanWrite:
		re.Reason = reasonRole
	case running:
		re.Reason = reasonRunning
	}
	out = append(out, re)

	if running {
		c := uxAction{Label: "İptal et", Post: base + "/cancel", Class: "danger", Confirm: sha + " deploy'u iptal edilsin mi?"}
		switch {
		case !x.CanWrite:
			c.Reason = reasonRole
		case d.CancelRequestedAt != nil:
			c.Reason = "İptal zaten istendi."
		}
		out = append(out, c)
	}

	rev := uxAction{Label: "İncele", Href: "/deployments/" + strconv.FormatInt(d.ID, 10) + "/review"}
	if !ready {
		rev.Reason = notReadyReason(d)
	}
	out = append(out, rev)

	if s.Terminal != nil {
		t := uxAction{Label: "Terminal", Href: terminalPath(app.Name, d.ID)}
		switch {
		case !x.CanWrite:
			t.Reason = reasonRole
		case terminalState(d) == termRetired:
			t.Reason = reasonTermRetired
		case !terminalOn:
			t.Reason = reasonTermBusy
		}
		out = append(out, t)
	}
	return out
}

// notReadyReason says why a deployment that is not ready cannot be opened.
func notReadyReason(d store.Deployment) string {
	switch {
	case d.Status == store.StatusRetired || d.RetiredAt != nil:
		return reasonRetired
	case d.Status == store.StatusFailed:
		return reasonFailed
	case d.Status == store.StatusCanceled:
		return reasonCanceled
	}
	return reasonNotReady
}

func (s *Server) deployURL(d store.Deployment) string {
	return naming.URL(s.Scheme, d.Host(s.Domain))
}

// ---- after an action ----

// backTo is where a deploy control returns: the deployment page or Genel
// when the form came from there, the Deploy'lar tab otherwise.
func backTo(r *http.Request, app store.App, id int64, flash string) string {
	switch r.PostFormValue("from") {
	case "deployment":
		if id > 0 {
			return fmt.Sprintf("/deployments/%d?ok=%s", id, flash)
		}
	case "app":
		return "/apps/" + app.Name + "?ok=" + flash
	}
	return "/apps/" + app.Name + "/deployments?ok=" + flash
}

// ---- Ayarlar: bulk .env ----

// parseDotenv reads KEY=value lines: blank lines and # comments are
// skipped, "export " is allowed, matching single or double quotes are
// removed (\n inside double quotes is a newline).
func parseDotenv(text string) ([]store.EnvChange, string) {
	var out []store.EnvChange
	seen := map[string]int{}
	for i, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Sprintf(envBulkLine, i+1, envBulkSyntax)
		}
		value = strings.TrimSpace(value)
		if n := len(value); n >= 2 && (value[0] == '"' && value[n-1] == '"' || value[0] == '\'' && value[n-1] == '\'') {
			q := value[0]
			value = value[1 : n-1]
			if q == '"' {
				value = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(value)
			}
		} else if j := strings.Index(value, " #"); j >= 0 {
			value = strings.TrimSpace(value[:j])
		}
		v := value
		if err := api.CheckEnvVar(key, &v); err != nil {
			return nil, fmt.Sprintf(envBulkLine, i+1, tr(err.Error()))
		}
		if j, dup := seen[key]; dup {
			out[j].Value = &v // the last value wins, like a shell
			continue
		}
		seen[key] = len(out)
		out = append(out, store.EnvChange{Key: key, Value: &v})
	}
	switch {
	case len(out) == 0:
		return nil, envBulkEmpty
	case len(out) > 200:
		return nil, envBulkTooMany
	}
	return out, ""
}

func (s *Server) setEnvBulk(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	target := strings.TrimSpace(r.PostFormValue("target"))
	branch := strings.TrimSpace(r.PostFormValue("git_branch"))
	if target != store.EnvPreview {
		branch = ""
	}
	status, msg, okMsg := http.StatusOK, "", ""
	changes, perr := parseDotenv(r.PostFormValue("dotenv"))
	if err := api.CheckEnvScope(target, branch); err != nil {
		status, msg = http.StatusBadRequest, tr(err.Error())
	} else if perr != "" {
		status, msg = http.StatusBadRequest, perr
	} else {
		for i := range changes {
			changes[i].Target, changes[i].GitBranch = target, branch
		}
		if err := s.Store.ApplyEnvChanges(r.Context(), app.ID, changes); err != nil {
			s.internalError(w, r, err)
			return
		}
		okMsg = fmt.Sprintf(envBulkSaved, len(changes))
		s.Log.Info("env updated", "app", app.Name, "changed", len(changes), "target", target, "via", "web bulk")
	}
	switch {
	case isHTMX(r):
		v, err := s.appDetail(r.Context(), app)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		v.EnvFlash = okMsg
		if msg != "" {
			v.EnvBulk = r.PostFormValue("dotenv") // keep what was pasted
		}
		s.partial(w, r, status, "env", v, msg)
	case msg != "":
		s.renderTabOf(w, r, app, status, tabSettings, msg, func(v *appDetail) { v.EnvBulk = r.PostFormValue("dotenv") })
	default:
		http.Redirect(w, r, "/apps/"+app.Name+"/settings?ok=env-bulk#env", http.StatusSeeOther)
	}
}
