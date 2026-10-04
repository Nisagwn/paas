package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

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
		s.errorPage(w, r, http.StatusForbidden, "This action needs the "+need+" role on the team.")
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
		rerender(http.StatusNotFound, "Not found. New members must be existing GitHub accounts.")
	case errors.Is(err, auth.ErrForbidden):
		rerender(http.StatusForbidden, "Only team owners can do this.")
	case errors.Is(err, store.ErrConflict):
		rerender(http.StatusConflict, "That name is already taken.")
	case errors.Is(err, store.ErrLastOwner):
		rerender(http.StatusConflict, "A team needs at least one owner.")
	default:
		s.internalError(w, r, err)
	}
	return true
}

// ---- teams ----

func (s *Server) teamsPage(w http.ResponseWriter, r *http.Request) {
	s.renderTeams(w, r, http.StatusOK, "", nil)
}

func (s *Server) renderTeams(w http.ResponseWriter, r *http.Request, status int, errMsg string, form map[string]string) {
	teams, err := s.Auth.Teams(r.Context(), me(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, r, status, "teams", "Teams", map[string]any{"Teams": teams, "Form": form}, errMsg)
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
	s.renderTeam(w, r, http.StatusOK, "")
}

func (s *Server) renderTeam(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	t, err := s.Auth.Team(r.Context(), me(r), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Team not found.")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	members, err := s.Store.TeamMembers(r.Context(), t.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	apps, err := s.Auth.Apps(r.Context(), me(r))
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
	s.render(w, r, status, "team", t.Name, map[string]any{
		"Team": t, "Members": members, "Apps": teamApps, "Owner": t.Role == store.RoleOwner,
		"Roles": []string{store.RoleViewer, store.RoleMember, store.RoleOwner},
	}, errMsg)
}

func (s *Server) setMember(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	login, role := strings.TrimSpace(r.PostFormValue("login")), r.PostFormValue("role")
	u, err := s.Auth.SetMember(r.Context(), me(r), slug, login, role)
	if s.opError(w, r, err, func(status int, msg string) { s.renderTeam(w, r, status, msg) }) {
		return
	}
	s.Log.Info("web: team member set", "team", slug, "user", u.Login, "role", role, "by", me(r).Login)
	http.Redirect(w, r, "/teams/"+slug+"?ok=member", http.StatusSeeOther)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	slug, login := r.PathValue("slug"), r.PostFormValue("login")
	err := s.Auth.RemoveMember(r.Context(), me(r), slug, login)
	if s.opError(w, r, err, func(status int, msg string) { s.renderTeam(w, r, status, msg) }) {
		return
	}
	s.Log.Info("web: team member removed", "team", slug, "user", login, "by", me(r).Login)
	if strings.EqualFold(login, me(r).Login) {
		http.Redirect(w, r, "/teams?ok=member", http.StatusSeeOther) // left the team
		return
	}
	http.Redirect(w, r, "/teams/"+slug+"?ok=member", http.StatusSeeOther)
}

// ---- personal API tokens ----

func (s *Server) tokensPage(w http.ResponseWriter, r *http.Request) {
	s.renderTokens(w, r, http.StatusOK, "", "")
}

// renderTokens shows the user's tokens; created is a new plain token,
// rendered once and never again.
func (s *Server) renderTokens(w http.ResponseWriter, r *http.Request, status int, errMsg, created string) {
	var tokens []store.APIToken
	if p := me(r); p.UserID != 0 {
		var err error
		if tokens, err = s.Store.APITokens(r.Context(), p.UserID); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	s.render(w, r, status, "tokens", "API tokens", map[string]any{
		"Tokens": tokens, "Created": created, "HasUser": me(r).UserID != 0,
	}, errMsg)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	days := 0
	if v := strings.TrimSpace(r.PostFormValue("expires_in_days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.renderTokens(w, r, http.StatusBadRequest, "Expiry must be a number of days.", "")
			return
		}
		days = n
	}
	t, plain, err := s.Auth.CreateToken(r.Context(), me(r), r.PostFormValue("name"), days)
	if s.opError(w, r, err, func(status int, msg string) { s.renderTokens(w, r, status, msg, "") }) {
		return
	}
	s.Log.Info("web: api token created", "user", me(r).Login, "token", t.Prefix)
	s.renderTokens(w, r, http.StatusCreated, "", plain)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderTokens(w, r, http.StatusBadRequest, "Invalid token.", "")
		return
	}
	err = s.Store.RevokeAPIToken(r.Context(), me(r).UserID, id)
	if s.opError(w, r, err, func(status int, msg string) { s.renderTokens(w, r, status, msg, "") }) {
		return
	}
	s.Log.Info("web: api token revoked", "user", me(r).Login, "id", id)
	http.Redirect(w, r, "/tokens?ok=revoked", http.StatusSeeOther)
}
