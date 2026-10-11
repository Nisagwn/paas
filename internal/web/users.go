package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 13: role checks, team management and personal API tokens.

func me(r *http.Request) auth.Principal {
	p, _ := auth.From(r.Context())
	return p
}

// checkTeam renders 404 (notFound) when the caller is not in the team, so
// other teams' names never leak, and 403 when the role is below need.
func (s *Server) checkTeam(w http.ResponseWriter, r *http.Request, teamID int64, need, notFound string) bool {
	role, err := s.Auth.TeamRole(r.Context(), me(r), teamID)
	switch {
	case err != nil:
		s.internalError(w, r, err)
		return false
	case role == "":
		s.errorPage(w, r, http.StatusNotFound, notFound)
		return false
	case !store.RoleAllows(role, need):
		s.errorPage(w, r, http.StatusForbidden, "Bu işlem için ekipte "+roleLabel(need)+" rolü gerekir.")
		return false
	}
	return true
}

// opError renders a failed team or token operation; false when err is nil.
func (s *Server) opError(w http.ResponseWriter, r *http.Request, err error, rerender func(int, string)) bool {
	if err == nil {
		return false
	}
	if msg, ok := auth.IsInvalid(err); ok {
		rerender(http.StatusBadRequest, msg)
		return true
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		rerender(http.StatusNotFound, "Bulunamadı. Yeni üyenin bir GitHub hesabı olmalı.")
	case errors.Is(err, auth.ErrForbidden):
		rerender(http.StatusForbidden, "Bunu yalnızca ekip sahipleri yapabilir.")
	case errors.Is(err, store.ErrConflict):
		rerender(http.StatusConflict, "Bu ad zaten kullanılıyor.")
	case errors.Is(err, store.ErrLastOwner):
		rerender(http.StatusConflict, "Ekipte en az bir sahip olmalı. Önce başka bir üyeyi sahip yap.")
	default:
		s.internalError(w, r, err)
	}
	return true
}

// ---- teams ----

// teamRow is one team of the Ekipler list.
type teamRow struct {
	store.Team
	Members, Apps int
	Plan          string
	Suspended     bool
}

func (s *Server) teamsPage(w http.ResponseWriter, r *http.Request) {
	s.renderTeams(w, r, http.StatusOK, "", nil)
}

func (s *Server) renderTeams(w http.ResponseWriter, r *http.Request, status int, errMsg string, form map[string]string) {
	ctx := r.Context()
	teams, err := s.Auth.Teams(ctx, me(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	apps, err := s.Auth.Apps(ctx, me(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	appCount := map[int64]int{}
	for _, a := range apps {
		appCount[a.TeamID]++
	}
	rows := make([]teamRow, 0, len(teams))
	for _, t := range teams {
		row := teamRow{Team: t, Apps: appCount[t.ID]}
		members, err := s.Store.TeamMembers(ctx, t.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		row.Members = len(members)
		if tp, err := s.Store.GetTeamPlan(ctx, t.ID); err == nil {
			row.Plan, row.Suspended = planLabel(tp.Plan), tp.Suspended()
		} else if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, err)
			return
		}
		rows = append(rows, row)
	}
	s.render(w, r, status, "teams", "Ekipler", map[string]any{
		"Teams": rows, "Form": form, "CanCreate": me(r).UserID != 0 || me(r).Admin,
	}, errMsg)
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	slug, name := strings.TrimSpace(r.PostFormValue("slug")), strings.TrimSpace(r.PostFormValue("name"))
	t, err := s.Auth.CreateTeam(r.Context(), me(r), slug, name)
	if s.opError(w, r, err, func(status int, msg string) {
		s.renderTeams(w, r, status, msg, map[string]string{"Slug": slug, "Name": name})
	}) {
		return
	}
	s.Log.Info("web: team created", "team", t.Slug, "by", me(r).Login)
	http.Redirect(w, r, "/teams/"+t.Slug+"?ok=team", http.StatusSeeOther)
}

func (s *Server) teamPage(w http.ResponseWriter, r *http.Request) {
	s.renderTeam(w, r, http.StatusOK, "", nil)
}

// memberRow is one member of the team page.
type memberRow struct {
	store.Member
	Me bool
	// LastOwner: the only owner, who cannot leave or be demoted.
	LastOwner bool
}

// installRow is a GitHub App installation linked to the team.
type installRow struct {
	store.Installation
	Org bool
}

// renderTeam shows the team page; invite keeps the add-member form after
// an error.
func (s *Server) renderTeam(w http.ResponseWriter, r *http.Request, status int, errMsg string, invite map[string]string) {
	ctx := r.Context()
	t, err := s.Auth.Team(ctx, me(r), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Ekip bulunamadı.")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	members, err := s.Store.TeamMembers(ctx, t.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	owners := 0
	var ownerLogins []string
	for _, m := range members {
		if m.Role == store.RoleOwner {
			owners++
			ownerLogins = append(ownerLogins, m.Login)
		}
	}
	rows := make([]memberRow, 0, len(members))
	var mine *memberRow
	for _, m := range members {
		rows = append(rows, memberRow{Member: m, Me: m.ID != 0 && m.ID == me(r).UserID,
			LastOwner: m.Role == store.RoleOwner && owners == 1})
	}
	for i := range rows {
		if rows[i].Me {
			mine = &rows[i]
		}
	}
	apps, err := s.Auth.Apps(ctx, me(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var teamApps []store.App
	for _, a := range apps {
		if a.TeamID == t.ID {
			teamApps = append(teamApps, a)
		}
	}
	installs, err := s.Store.ListInstallations(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var teamInstalls []installRow
	for _, in := range installs {
		if in.TeamID != nil && *in.TeamID == t.ID {
			teamInstalls = append(teamInstalls, installRow{Installation: in, Org: in.AccountType == "Organization"})
		}
	}
	st, err := api.PlansFor(s.Plans, s.Store).Status(ctx, t.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	canWrite := store.RoleAllows(t.Role, store.RoleMember)
	installWhy := ""
	switch {
	case !s.installEnabled():
		installWhy = "GitHub uygulaması bu sunucuda yapılandırılmamış (PAAS_GITHUB_APP_SLUG ve GitHub ile giriş gerekir)."
	case me(r).UserID == 0:
		installWhy = "GitHub bağlamak için admin token'ı yerine GitHub ile giriş yap."
	case !canWrite:
		installWhy = "GitHub bağlamak için ekipte en az Üye rolü gerekir."
	}
	if invite == nil {
		invite = map[string]string{"Role": store.RoleMember}
	}
	s.render(w, r, status, "team", t.Name, map[string]any{
		"Team": t, "Members": rows, "Mine": mine, "Apps": teamApps, "Owner": t.Role == store.RoleOwner,
		"CanWrite": canWrite, "Owners": ownerLogins,
		"Roles":    []string{store.RoleViewer, store.RoleMember, store.RoleOwner},
		"RoleRows": roleCapabilities, "Installs": teamInstalls, "InstallWhy": installWhy,
		"PlanLabel": planLabel(st.Plan), "Status": st, "Invite": invite,
	}, errMsg)
}

func (s *Server) setMember(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	login, role := strings.TrimSpace(r.PostFormValue("login")), r.PostFormValue("role")
	change := r.PostFormValue("op") == "role"
	u, err := s.Auth.SetMember(r.Context(), me(r), slug, login, role)
	if s.opError(w, r, err, func(status int, msg string) {
		var invite map[string]string
		if !change {
			invite = map[string]string{"Login": login, "Role": role}
		}
		s.renderTeam(w, r, status, msg, invite)
	}) {
		return
	}
	s.Log.Info("web: team member set", "team", slug, "user", u.Login, "role", role, "by", me(r).Login)
	ok := "member-added"
	if change {
		ok = "member-role"
	}
	http.Redirect(w, r, "/teams/"+slug+"?ok="+ok+"#members", http.StatusSeeOther)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	slug, login := r.PathValue("slug"), r.PostFormValue("login")
	err := s.Auth.RemoveMember(r.Context(), me(r), slug, login)
	if s.opError(w, r, err, func(status int, msg string) { s.renderTeam(w, r, status, msg, nil) }) {
		return
	}
	s.Log.Info("web: team member removed", "team", slug, "user", login, "by", me(r).Login)
	if strings.EqualFold(strings.TrimPrefix(login, "@"), me(r).Login) {
		redirect(w, r, "/teams?ok=left-team") // left the team
		return
	}
	redirect(w, r, "/teams/"+slug+"?ok=member-removed#members")
}

// ---- personal API tokens ----

// tokenExpiries are the choices of the token form, in days (0: never).
var tokenExpiries = []struct {
	Days  int
	Label string
}{{7, "7 gün"}, {30, "30 gün"}, {90, "90 gün"}, {365, "1 yıl"}, {0, "Süresiz"}}

// tokenRow is one token of the list.
type tokenRow struct {
	store.APIToken
	Expired bool
	// Soon: expires within 7 days.
	Soon bool
}

func (s *Server) tokensPage(w http.ResponseWriter, r *http.Request) {
	s.renderTokens(w, r, http.StatusOK, "", "", nil)
}

// renderTokens shows the user's tokens; created is a new plain token,
// rendered once and never again. form keeps the inputs after an error.
func (s *Server) renderTokens(w http.ResponseWriter, r *http.Request, status int, errMsg, created string, form map[string]string) {
	var rows []tokenRow
	if p := me(r); p.UserID != 0 {
		tokens, err := s.Store.APITokens(r.Context(), p.UserID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		now := time.Now()
		for _, t := range tokens {
			row := tokenRow{APIToken: t}
			if t.ExpiresAt != nil {
				row.Expired = !t.ExpiresAt.After(now)
				row.Soon = !row.Expired && t.ExpiresAt.Before(now.Add(7*24*time.Hour))
			}
			rows = append(rows, row)
		}
	}
	if form == nil {
		form = map[string]string{"Days": "90"}
	}
	s.render(w, r, status, "tokens", "Token'lar", map[string]any{
		"Tokens": rows, "Created": created, "HasUser": me(r).UserID != 0,
		"Expiries": tokenExpiries, "Form": form, "Origin": requestOrigin(r),
	}, errMsg)
}

// requestOrigin is the platform's own address as the browser sees it, for
// the CLI and curl examples.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	days := 0
	v := strings.TrimSpace(r.PostFormValue("expires_in_days"))
	form := map[string]string{"Name": r.PostFormValue("name"), "Days": v}
	if v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.renderTokens(w, r, http.StatusBadRequest, "Süre gün sayısı olmalı.", "", form)
			return
		}
		days = n
	}
	t, plain, err := s.Auth.CreateToken(r.Context(), me(r), r.PostFormValue("name"), days)
	if s.opError(w, r, err, func(status int, msg string) { s.renderTokens(w, r, status, msg, "", form) }) {
		return
	}
	s.Log.Info("web: api token created", "user", me(r).Login, "token", t.Prefix)
	s.renderTokens(w, r, http.StatusCreated, "", plain, nil)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderTokens(w, r, http.StatusBadRequest, "Geçersiz token.", "", nil)
		return
	}
	err = s.Store.RevokeAPIToken(r.Context(), me(r).UserID, id)
	if s.opError(w, r, err, func(status int, msg string) { s.renderTokens(w, r, status, msg, "", nil) }) {
		return
	}
	s.Log.Info("web: api token revoked", "user", me(r).Login, "id", id)
	redirect(w, r, "/tokens?ok=revoked")
}
