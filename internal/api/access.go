package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 13: authentication and role-based authorization for /api/*.
//
// Roles are per team; an app belongs to one team:
//
//	viewer  read: apps, deployments, build/runtime logs, SSE streams, env keys
//	member  + deploy-affecting changes: rollback, env, creating apps
//	owner   + team management (members, roles)
//
// Apps and deployments of teams the caller is not in answer 404, exactly
// like ones that do not exist, so names never leak. A visible app with too
// low a role answers 403.
//
// Protecting a per-app endpoint ({name} in the pattern) takes one wrapper:
//
//	api.HandleFunc("POST /api/apps/{name}/domains", s.requireApp(store.RoleMember, s.addDomain))
//
// The handler then calls s.lookupApp(w, r) as before and gets the app the
// wrapper already checked. An unwrapped handler that calls lookupApp is
// still protected: it requires viewer for GET/HEAD and member otherwise.

type appCtxKey struct{}

// authenticate resolves the caller (bearer token, or the session cookie for
// read-only requests: EventSource cannot send headers, and unsafe methods
// would need a CSRF check) and stores it in the request context.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			p   auth.Principal
			err = auth.ErrUnauthenticated
		)
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			p, err = s.Auth.Bearer(r.Context(), token)
		} else if !s.cookieless && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			p, err = s.Auth.Session(r)
		}
		if errors.Is(err, auth.ErrUnauthenticated) {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// principal returns the authenticated caller; authenticate guarantees one.
func principal(r *http.Request) auth.Principal {
	p, _ := auth.From(r.Context())
	return p
}

// requireApp resolves the app named by {name}, checks that the caller has
// at least role need on its team, and hands the app to h via lookupApp.
func (s *Server) requireApp(need string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, ok := s.authorizeApp(w, r, need)
		if !ok {
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), appCtxKey{}, app)))
	}
}

// authorizeApp loads the {name} app and checks the caller's role on it.
func (s *Server) authorizeApp(w http.ResponseWriter, r *http.Request, need string) (store.App, bool) {
	app, err := s.Store.GetAppByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "app not found")
		return app, false
	}
	if err != nil {
		s.internalError(w, err)
		return app, false
	}
	return app, s.checkTeam(w, r, app.TeamID, need, "app not found")
}

// checkTeam answers 404 (notFound) when the caller is not in the team and
// 403 when its role is below need.
func (s *Server) checkTeam(w http.ResponseWriter, r *http.Request, teamID int64, need, notFound string) bool {
	role, err := s.Auth.TeamRole(r.Context(), principal(r), teamID)
	switch {
	case err != nil:
		s.internalError(w, err)
		return false
	case role == "":
		writeError(w, http.StatusNotFound, notFound)
		return false
	case !store.RoleAllows(role, need):
		writeError(w, http.StatusForbidden, "this action needs the "+need+" role on the team")
		return false
	}
	return true
}

// lookupDeployment loads the {id} deployment if the caller may read its app.
func (s *Server) lookupDeployment(w http.ResponseWriter, r *http.Request) (store.Deployment, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return store.Deployment{}, false
	}
	d, err := s.Store.GetDeployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment not found")
		return d, false
	}
	if err != nil {
		s.internalError(w, err)
		return d, false
	}
	app, err := s.Store.GetAppByName(r.Context(), d.AppName)
	if err != nil {
		s.internalError(w, err)
		return d, false
	}
	return d, s.checkTeam(w, r, app.TeamID, store.RoleViewer, "deployment not found")
}

// defaultRole is what an unwrapped per-app handler requires.
func defaultRole(method string) string {
	if method == http.MethodGet || method == http.MethodHead {
		return store.RoleViewer
	}
	return store.RoleMember
}
