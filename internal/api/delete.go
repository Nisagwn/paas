package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/nisagwn/paas/internal/appdelete"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 28: deleting a project.
//
//	DELETE /api/apps/{name}  {"confirm": "<name>"}
//
// Team owners, platform admins and the legacy admin token may delete; other
// members and viewers get 403, other teams 404. The body must repeat the
// app's name (400 otherwise). The answer is 204 when everything is gone
// already (nothing was running and the namespace is deleted), 202 when
// running deployments are still stopping or the namespace is still
// terminating: the app is gone from the API at once and the background loop
// (appdelete.Service.Run) finishes within seconds; the name can be used
// again after that.

// MsgAppDeleting answers creating or importing an app whose name belongs to
// a project that is still being deleted.
const MsgAppDeleting = "a project with this name is still being deleted; try again in a minute"

type deleteAppRequest struct {
	Confirm string `json:"confirm"`
}

// deleteAppView is the 202 body.
type deleteAppView struct {
	App        string `json:"app"`
	Status     string `json:"status"` // "deleting"
	DeletionID int64  `json:"deletion_id"`
}

// Deleter returns the project deletion service (one without a cluster when
// none was configured, e.g. in tests).
func (s *Server) deleter() *appdelete.Service {
	if s.Deleter != nil {
		return s.Deleter
	}
	return &appdelete.Service{Store: s.Store, Log: s.Log}
}

// CanDeleteApp reports whether p may delete an app of team teamID: its
// owners and platform admins.
func CanDeleteApp(ctx context.Context, a *auth.Authenticator, p auth.Principal, teamID int64) (bool, error) {
	if a.PlatformAdmin(p) {
		return true, nil
	}
	role, err := a.TeamRole(ctx, p, teamID)
	return role == store.RoleOwner, err
}

// Actor is who a deletion is recorded for.
func Actor(p auth.Principal) store.DeletionActor {
	a := store.DeletionActor{Login: p.Login}
	if p.UserID != 0 {
		id := p.UserID
		a.UserID = &id
	}
	return a
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.Store.GetAppByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	p := principal(r)
	if !s.Auth.PlatformAdmin(p) && !s.checkTeam(w, r, app.TeamID, store.RoleOwner, "app not found") {
		return
	}
	var req deleteAppRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, `body must be {"confirm": "`+app.Name+`"}`)
		return
	}
	res, err := s.deleter().Delete(r.Context(), app, req.Confirm, Actor(p))
	switch {
	case errors.Is(err, appdelete.ErrConfirm):
		writeError(w, http.StatusBadRequest, `"confirm" must be the app name ("`+app.Name+`")`)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "app not found")
	case err != nil:
		s.internalError(w, err)
	case res.Done:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusAccepted, deleteAppView{App: app.Name, Status: "deleting", DeletionID: res.Deletion.ID})
	}
}
