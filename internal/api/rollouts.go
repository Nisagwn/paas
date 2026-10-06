package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/rollout"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 21: gradual rollouts (canary) and automatic rollback.
//
//	GET  /api/apps/{name}/rollout-settings          viewer
//	PUT  /api/apps/{name}/rollout-settings          member
//	GET  /api/apps/{name}/rollout                   viewer: active + recent, with per-side metrics
//	POST /api/apps/{name}/rollout/{action}          member: promote, abort, pause, resume, rollback

// rolloutSettingsRequest changes some settings; omitted fields keep their
// current value.
type rolloutSettingsRequest struct {
	Mode                *string  `json:"mode"`
	Steps               *[]int   `json:"steps"`
	StepSeconds         *int     `json:"step_seconds"`
	MaxErrorPct         *float64 `json:"max_error_pct"`
	MaxErrorIncreasePct *float64 `json:"max_error_increase_pct"`
	MaxP95Ms            *int     `json:"max_p95_ms"`
	MaxP95Factor        *float64 `json:"max_p95_factor"`
	MinRequests         *int     `json:"min_requests"`
	GuardSeconds        *int     `json:"guard_seconds"`
}

// Apply merges the request into s.
func (q rolloutSettingsRequest) Apply(s store.RolloutSettings) store.RolloutSettings {
	set := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	setF := func(dst *float64, v *float64) {
		if v != nil {
			*dst = *v
		}
	}
	if q.Mode != nil {
		s.Mode = *q.Mode
	}
	if q.Steps != nil {
		s.Steps = *q.Steps
	}
	set(&s.StepSeconds, q.StepSeconds)
	setF(&s.MaxErrorPct, q.MaxErrorPct)
	setF(&s.MaxErrorIncreasePct, q.MaxErrorIncreasePct)
	set(&s.MaxP95Ms, q.MaxP95Ms)
	setF(&s.MaxP95Factor, q.MaxP95Factor)
	set(&s.MinRequests, q.MinRequests)
	set(&s.GuardSeconds, q.GuardSeconds)
	return s
}

func (s *Server) getRolloutSettings(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	rs, err := s.Store.GetRolloutSettings(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (s *Server) putRolloutSettings(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var req rolloutSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cur, err := s.Store.GetRolloutSettings(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	next := req.Apply(cur)
	if err := next.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := s.Store.SetRolloutSettings(r.Context(), app.ID, next)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("rollout settings", "app", app.Name, "mode", saved.Mode, "steps", store.FormatSteps(saved.Steps),
		"step_seconds", saved.StepSeconds, "by", principal(r).Login)
	writeJSON(w, http.StatusOK, saved)
}

// RolloutMetrics are both sides of a rollout over its current window.
type RolloutMetrics struct {
	WindowFrom time.Time `json:"window_from"`
	// Canary: the new deployment. Production: the current production
	// (canary) or the previous one before the switch (guard).
	Canary     store.RolloutSample `json:"canary"`
	Production store.RolloutSample `json:"production"`
	// Next is what the controller would decide now.
	Next     rollout.Action `json:"next_action"`
	NextWhy  string         `json:"next_reason"`
	Elapsed  int64          `json:"elapsed_seconds"`
	Duration int64          `json:"duration_seconds"`
}

// RolloutView is a rollout with the URLs of both deployments and, for the
// active one, its metrics.
type RolloutView struct {
	store.Rollout
	FromURL string          `json:"from_url,omitempty"`
	ToURL   string          `json:"to_url"`
	Metrics *RolloutMetrics `json:"metrics,omitempty"`
}

// RolloutStatus is the answer of GET /rollout.
type RolloutStatus struct {
	Settings store.RolloutSettings `json:"settings"`
	Active   *RolloutView          `json:"active"`
	Recent   []RolloutView         `json:"recent"`
}

// LoadRolloutStatus gathers an app's rollout status (also used by the web UI).
func LoadRolloutStatus(ctx context.Context, st *store.Store, app store.App, scheme, domain string, now time.Time) (RolloutStatus, error) {
	var out RolloutStatus
	var err error
	if out.Settings, err = st.GetRolloutSettings(ctx, app.ID); err != nil {
		return out, err
	}
	list, err := st.ListRollouts(ctx, app.ID, 10)
	if err != nil {
		return out, err
	}
	urls := map[int64]string{}
	url := func(id int64) string {
		if u, ok := urls[id]; ok {
			return u
		}
		d, err := st.GetDeployment(ctx, id)
		if err != nil {
			return ""
		}
		urls[id] = naming.URL(scheme, d.Host(domain))
		return urls[id]
	}
	out.Recent = []RolloutView{}
	for _, r := range list {
		v := RolloutView{Rollout: r, ToURL: url(r.ToDeploymentID)}
		if r.FromDeploymentID != nil {
			v.FromURL = url(*r.FromDeploymentID)
		}
		if r.Active() {
			c := &rollout.Controller{Store: st}
			in, err := c.Input(ctx, r, now)
			if err != nil {
				return out, err
			}
			next := rollout.Decide(in)
			from := r.StepStartedAt
			v.Metrics = &RolloutMetrics{
				WindowFrom: from, Canary: in.Canary, Production: in.Baseline, Next: next.Action, NextWhy: next.Reason,
				Elapsed: int64(in.Elapsed.Seconds()), Duration: int64(in.Duration.Seconds()),
			}
			active := v
			out.Active = &active
		}
		out.Recent = append(out.Recent, v)
	}
	return out, nil
}

func (s *Server) getRollout(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	out, err := LoadRolloutStatus(r.Context(), s.Store, app, s.Scheme, s.Domain, s.clock())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// RolloutAction applies a user's action to the app's active rollout,
// logs it to the new deployment's log and applies the routes. The error
// is store.ErrNotFound (no active rollout), store.ErrInvalid (the action
// does not apply now), store.ErrRetired/ErrNotReady (the target of an
// alias move is gone) or a sync error after the change was saved
// (errRouteSync).
func RolloutAction(ctx context.Context, st *store.Store, router Router, app store.App, action, by string) (store.Rollout, error) {
	ro, err := st.ManualRolloutAction(ctx, app.ID, action, by)
	if err != nil {
		return ro, err
	}
	st.AppendLog(ctx, ro.ToDeploymentID, "==> "+rolloutActionLog[action]+" ("+by+")")
	if router != nil {
		if err := router.SyncApp(ctx, app.Name); err != nil {
			return ro, errors.Join(ErrRouteSync, err)
		}
	}
	return ro, nil
}

// ErrRouteSync: the change was saved but applying the routes failed (the
// reconcile loop retries).
var ErrRouteSync = errors.New("saved, but updating the router failed (retried automatically)")

var rolloutActionLog = map[string]string{
	store.ActionPromote:  "yayın elle tamamlandı: production artık bu deploy",
	store.ActionAbort:    "yayın elle durduruldu; production değişmedi",
	store.ActionPause:    "yayın duraklatıldı",
	store.ActionResume:   "yayın sürdürülüyor",
	store.ActionRollback: "yayın elle geri alındı",
}

func (s *Server) rolloutAction(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if _, ok := rolloutActionLog[action]; !ok {
		writeError(w, http.StatusNotFound, "unknown action: use promote, abort, pause, resume or rollback")
		return
	}
	ro, err := RolloutAction(r.Context(), s.Store, s.Router, app, action, principal(r).Login)
	switch {
	case errors.Is(err, ErrRouteSync):
		s.Log.Error("rollout action: route sync", "app", app.Name, "err", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no active rollout for this app")
		return
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusConflict, "cannot "+action+" a rollout that is "+ro.State)
		return
	case errors.Is(err, store.ErrRetired), errors.Is(err, store.ErrNotReady):
		writeError(w, http.StatusConflict, "the target deployment does not run any more: "+err.Error())
		return
	case errors.Is(err, store.ErrStale):
		writeError(w, http.StatusConflict, "the rollout changed meanwhile; reload and try again")
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	s.Log.Info("rollout action", "app", app.Name, "rollout", ro.ID, "action", action, "state", ro.State,
		"by", principal(r).Login)
	writeJSON(w, http.StatusOK, ro)
}
