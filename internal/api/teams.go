package api

import (
	"errors"
	"net/http"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 13: the caller, teams, members and personal API tokens.

type meView struct {
	ID    int64        `json:"id,omitempty"`
	Login string       `json:"login"`
	Name  string       `json:"name,omitempty"`
	Admin bool         `json:"admin"`
	Teams []store.Team `json:"teams"`
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	teams, err := s.Auth.Teams(r.Context(), p)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, meView{ID: p.UserID, Login: p.Login, Name: p.Name, Admin: p.Admin, Teams: teams})
}

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := s.Auth.Teams(r.Context(), principal(r))
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, teams)
}

type createTeamRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	var req createTeamRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := s.Auth.CreateTeam(r.Context(), principal(r), req.Slug, req.Name)
	if s.teamError(w, err, "team") {
		return
	}
	s.Log.Info("team created", "team", t.Slug, "by", principal(r).Login)
	writeJSON(w, http.StatusCreated, t)
}

type teamView struct {
	store.Team
	Members []store.Member `json:"members"`
}

func (s *Server) getTeam(w http.ResponseWriter, r *http.Request) {
	t, err := s.Auth.Team(r.Context(), principal(r), r.PathValue("slug"))
	if s.teamError(w, err, "team") {
		return
	}
	members, err := s.Store.TeamMembers(r.Context(), t.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, teamView{Team: t, Members: members})
}

type memberRequest struct {
	Role string `json:"role"`
}

// setMember adds a GitHub user to the team or changes their role (owner).
func (s *Server) setMember(w http.ResponseWriter, r *http.Request) {
	var req memberRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	slug, login := r.PathValue("slug"), r.PathValue("login")
	u, err := s.Auth.SetMember(r.Context(), principal(r), slug, login, req.Role)
	if s.teamError(w, err, "team or GitHub user") {
		return
	}
	s.Log.Info("team member set", "team", slug, "user", u.Login, "role", req.Role, "by", principal(r).Login)
	writeJSON(w, http.StatusOK, store.Member{User: u, Role: req.Role})
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	slug, login := r.PathValue("slug"), r.PathValue("login")
	err := s.Auth.RemoveMember(r.Context(), principal(r), slug, login)
	if s.teamError(w, err, "team or member") {
		return
	}
	s.Log.Info("team member removed", "team", slug, "user", login, "by", principal(r).Login)
	w.WriteHeader(http.StatusNoContent)
}

// teamError writes the response for a failed team or token operation and
// reports whether there was one.
func (s *Server) teamError(w http.ResponseWriter, err error, what string) bool {
	if err == nil {
		return false
	}
	if msg, ok := auth.IsInvalid(err); ok {
		writeError(w, http.StatusBadRequest, msg)
		return true
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, what+" not found")
	case errors.Is(err, auth.ErrForbidden):
		writeError(w, http.StatusForbidden, "this action needs the owner role on the team")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, what+" already exists")
	case errors.Is(err, store.ErrLastOwner):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.internalError(w, err)
	}
	return true
}

// ---- personal API tokens ----

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.UserID == 0 {
		writeJSON(w, http.StatusOK, []store.APIToken{})
		return
	}
	tokens, err := s.Store.APITokens(r.Context(), p.UserID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

type createTokenRequest struct {
	Name          string `json:"name"`
	ExpiresInDays int    `json:"expires_in_days"`
}

type createdToken struct {
	store.APIToken
	// Token is the only time the plain token is shown.
	Token string `json:"token"`
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req createTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, plain, err := s.Auth.CreateToken(r.Context(), principal(r), req.Name, req.ExpiresInDays)
	if s.teamError(w, err, "token") {
		return
	}
	s.Log.Info("api token created", "user", principal(r).Login, "token", t.Prefix)
	writeJSON(w, http.StatusCreated, createdToken{APIToken: t, Token: plain})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.Store.RevokeAPIToken(r.Context(), principal(r).UserID, id)
	if s.teamError(w, err, "token") {
		return
	}
	s.Log.Info("api token revoked", "user", principal(r).Login, "id", id)
	w.WriteHeader(http.StatusNoContent)
}
