package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/webhook"
)

// pullRequestClosed treats a closed pull request like a deletion of its
// head branch: the preview has served its purpose.
func (s *Server) pullRequestClosed(w http.ResponseWriter, r *http.Request, body []byte) {
	pr, err := webhook.ParseClosedPR(body)
	if errors.Is(err, webhook.ErrIgnored) {
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored", "reason": "not a closed pull request from this repository"})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.branchDeleted(w, r, pr.Repo, pr.Branch, fmt.Sprintf("pull request #%d closed", pr.Number))
}

// branchDeleted removes the branch's preview alias and retires its
// deployments in the database, then kicks the collector to sync routes and
// delete the cluster objects in the background (the periodic sweep does it
// otherwise). The production branch is never cleaned this way.
func (s *Server) branchDeleted(w http.ResponseWriter, r *http.Request, repo, branch, reason string) {
	app, err := s.Store.GetAppByRepo(r.Context(), repo)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored", "reason": "no app for repo " + repo})
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	if branch == app.ProductionBranch {
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored", "reason": "production branch is never cleaned up"})
		return
	}
	bc, err := s.Store.DeleteBranch(r.Context(), app.ID, branch, reason)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if s.Cleanup != nil {
		s.Cleanup.Kick(app.Name)
	}
	s.Log.Info("branch cleanup", "app", app.Name, "branch", branch, "reason", reason,
		"aliases_removed", bc.AliasesRemoved, "retired", bc.Retired, "cancelled", bc.Cancelled, "in_flight", bc.InFlight)
	writeJSON(w, http.StatusOK, struct {
		Result string `json:"result"`
		Branch string `json:"branch"`
		store.BranchCleanup
	}{reason, branch, bc})
}
