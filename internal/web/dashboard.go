package web

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// The dashboard ("Projeler"): project cards, search, the team filter of the
// header switcher, and the onboarding checklist of a new user.

// appRow is one project card.
type appRow struct {
	store.App
	Team          string
	TeamName      string
	ProductionURL string
	// Production is the deployment the production alias serves (nil: the
	// project is not live yet). Latest is the newest deployment of any
	// branch.
	Production *store.Deployment
	Latest     *store.Deployment
	// Framework is the display name of the last detected framework.
	Framework string
}

// Active reports whether the newest deployment is still running.
func (a appRow) Active() bool {
	return a.Latest != nil && !a.Latest.Finished()
}

// LatestIsProduction: the newest deployment is the one production serves.
func (a appRow) LatestIsProduction() bool {
	return a.Latest != nil && a.Production != nil && a.Latest.ID == a.Production.ID
}

// lastActivity orders cards: newest deployment first, then newest project.
func (a appRow) lastActivity() time.Time {
	if a.Latest != nil {
		return a.Latest.CreatedAt
	}
	return a.CreatedAt
}

// onboardingStep is one item of the first-use checklist.
type onboardingStep struct {
	Title, Text string
	// State: done, current (the next step), todo or skip (not available
	// on this server).
	State string
	// Action is the button of the current step.
	Href, Action string
}

// onboarding is the checklist shown until the first production deploy.
type onboarding struct {
	Steps []onboardingStep
	Done  int
}

func (s *Server) appsPage(w http.ResponseWriter, r *http.Request) {
	rememberTeam(w, r)
	s.renderApps(w, r, http.StatusOK, "")
}

func (s *Server) renderApps(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	ctx := r.Context()
	me, _ := auth.From(ctx)
	apps, err := s.Auth.Apps(ctx, me)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	teams, err := s.Auth.Teams(ctx, me)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	byID := map[int64]store.Team{}
	var writable []store.Team
	var teamIDs []int64
	for _, t := range teams {
		byID[t.ID] = t
		teamIDs = append(teamIDs, t.ID)
		if store.RoleAllows(t.Role, store.RoleMember) {
			writable = append(writable, t)
		}
	}
	selected := pickTeam(teams, selectedSlug(r))
	latest, err := s.Store.LatestDeployments(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	prod, err := s.Store.ProductionDeployments(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	everything := make([]appRow, 0, len(apps))
	all := make([]appRow, 0, len(apps))
	live := false
	for _, a := range apps {
		t := byID[a.TeamID]
		row := appRow{App: a, Team: t.Slug, TeamName: t.Name,
			ProductionURL: naming.URL(s.Scheme, naming.ProductionHost(a.Name, s.Domain))}
		if d, ok := latest[a.ID]; ok {
			row.Latest = &d
			row.Framework = frameworkName(d.Framework)
		}
		if d, ok := prod[a.ID]; ok {
			row.Production = &d
			live = true
			if d.Framework != "" {
				row.Framework = frameworkName(d.Framework)
			}
		}
		everything = append(everything, row)
		if selected == nil || a.TeamID == selected.ID {
			all = append(all, row)
		}
	}
	sort.SliceStable(everything, func(i, j int) bool {
		return everything[i].lastActivity().After(everything[j].lastActivity())
	})
	sort.SliceStable(all, func(i, j int) bool { return all[i].lastActivity().After(all[j].lastActivity()) })

	rows := all
	if q != "" {
		rows = make([]appRow, 0, len(all))
		needle := strings.ToLower(q)
		for _, a := range all {
			if strings.Contains(strings.ToLower(a.Name+" "+a.Repo+" "+a.TeamName), needle) {
				rows = append(rows, a)
			}
		}
	}
	polling := false
	for _, a := range rows {
		if a.Active() {
			polling = true
		}
	}

	var ob *onboarding
	if !live {
		installed, err := s.Store.TeamsHaveInstallation(ctx, teamIDs)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		ob = s.onboarding(me, writable, installed, len(apps) > 0, everything)
	}

	s.render(w, r, status, "apps", "Projeler", map[string]any{
		"Apps": rows, "Total": len(all), "Query": q, "Team": selected,
		"MultiTeam": len(teams) > 1, "CanCreate": len(writable) > 0,
		"Polling": polling, "Onboarding": ob,
	}, errMsg)
}

// onboarding builds the first-use checklist from the real state: GitHub
// connected, a project imported, a production deployment.
func (s *Server) onboarding(p auth.Principal, writable []store.Team, installed, hasApps bool, apps []appRow) *onboarding {
	ob := &onboarding{}
	connect := onboardingStep{Title: obConnectTitle, Text: obConnectText, Href: "/import", Action: obConnectAction}
	switch {
	case installed:
		connect.State = "done"
	case s.GitHubAppSlug == "" || s.GitHub == nil || p.UserID == 0:
		connect.State, connect.Text = "skip", obConnectSkip
	default:
		connect.State = "todo"
		if len(writable) == 1 {
			connect.Href = "/github/install?team=" + writable[0].Slug
		}
	}
	pick := onboardingStep{Title: obPickTitle, Text: obPickText, Href: "/import", Action: obPickAction, State: "todo"}
	if hasApps {
		pick.State = "done"
	}
	deploy := onboardingStep{Title: obDeployTitle, Text: obDeployText, State: "todo"}
	for _, a := range apps {
		if a.Latest == nil {
			continue
		}
		deploy.Href, deploy.Action = "/deployments/"+itoa64(a.Latest.ID), obDeployWatch
		if a.Latest.Status == store.StatusFailed {
			deploy.Text, deploy.Action = obDeployFailed, obDeployLogs
		}
		break
	}
	if deploy.Href == "" && hasApps {
		deploy.Href, deploy.Action = "/apps/"+apps[0].Name, obDeployOpen
		deploy.Text = obDeployPush(apps[0].ProductionBranch)
	}
	ob.Steps = []onboardingStep{connect, pick, deploy}
	next := false
	for i := range ob.Steps {
		st := &ob.Steps[i]
		if st.State == "done" || st.State == "skip" {
			ob.Done++
			continue
		}
		if !next {
			st.State, next = "current", true
		}
	}
	return ob
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	if !s.allowImport(w, r) {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	repo := strings.TrimSpace(r.PostFormValue("repo"))
	branch := strings.TrimSpace(r.PostFormValue("production_branch"))
	if branch == "" {
		branch = "main"
	}
	teamSlug := strings.TrimSpace(r.PostFormValue("team"))
	form := manualForm{Name: name, Repo: repo, Branch: branch, Team: teamSlug, Open: true}
	fail := func(status int, msg string, help *errorHelp) {
		s.renderImport(w, r, status, importState{Manual: form, Error: msg, Help: help})
	}
	switch {
	case !naming.ValidAppName(name):
		fail(http.StatusBadRequest, "Ad 2-31 karakter olmalı: a-z harfleri, rakamlar ve '-'; harfle başlamalı.", nil)
		return
	case !api.ValidRepo(repo):
		fail(http.StatusBadRequest, `Repo "sahip/repo" biçiminde olmalı.`, nil)
		return
	}
	me, _ := auth.From(r.Context())
	team, err := s.Auth.TeamForApps(r.Context(), me, teamSlug)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, auth.ErrForbidden) {
		fail(http.StatusForbidden, "Proje eklemek için ekipte üye rolü gerekir.", &helpRole)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.Plans.CheckCreateApp(r.Context(), team.ID); err != nil {
		if status, msg, ok := limitError(err); ok {
			fail(status, msg, &helpLimit)
			return
		}
		s.internalError(w, r, err)
		return
	}
	_, err = s.Store.CreateAppInTeam(r.Context(), team.ID, name, repo, branch)
	if errors.Is(err, store.ErrConflict) {
		fail(http.StatusConflict, "Bu ad ya da repo ile bir proje zaten var.", &helpName)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("web: app created", "app", name, "repo", repo)
	http.Redirect(w, r, "/apps/"+name+"?ok=created", http.StatusSeeOther)
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
