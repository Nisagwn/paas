package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/ratelimit"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 23: plans, limits and abuse protection.
//
//	GET  /api/teams/{slug}/usage                  viewer of the team
//	GET  /api/admin/teams                         platform admin
//	GET  /api/admin/teams/{slug}                  platform admin
//	PUT  /api/admin/teams/{slug}/plan             platform admin  {"plan": "pro", "limits": {"projects": 50}}
//	POST /api/admin/teams/{slug}/suspend          platform admin  {"reason": "..."}
//	POST /api/admin/teams/{slug}/unsuspend        platform admin
//	GET  /api/admin/users/suspended               platform admin
//	POST /api/admin/users/{login}/suspend         platform admin  {"reason": "..."}
//	POST /api/admin/users/{login}/unsuspend       platform admin
//
// A platform admin is the legacy admin token or a user listed in
// PAAS_ADMIN_GITHUB_LOGINS (auth.Authenticator.PlatformAdmin).
//
// Limit refusals answer 403 (counts: projects, domains, databases,
// replicas; a suspended team) or 429 (windows that reset: deployments per
// day, build minutes per month) with
//
//	{"error": "...", "limit": "projects", "plan": "hobby", "max": 10, "used": 10}
//
// Rate limits (internal/ratelimit) answer 429 with Retry-After.

// Suspender scales a suspended team's apps to zero and back
// (deploy.Kubernetes); nil (dry run) only records the suspension.
type Suspender interface {
	SuspendApp(ctx context.Context, app string) error
	ResumeApp(ctx context.Context, app string) error
}

// RefusalReporter tells GitHub that a pushed commit was not deployed
// because of a plan limit (a failed commit status; github.Notifier).
type RefusalReporter interface {
	DeploymentRefused(ctx context.Context, repo, sha, reason string) error
}

// planError writes a *plans.LimitError and reports whether err was one.
func planError(w http.ResponseWriter, err error) bool {
	le := plans.AsLimit(err)
	if le == nil {
		return false
	}
	writeJSON(w, le.Status, le)
	return true
}

// PlansFor returns the configured Guard, or one with the default catalog
// for read-only views when plans are not enforced.
func PlansFor(g *plans.Guard, st *store.Store) *plans.Guard {
	if g != nil {
		return g
	}
	return &plans.Guard{Store: st, Catalog: plans.DefaultCatalog()}
}

// ---- rate limits ----

// limitWrites applies the API write rate limit to state-changing requests,
// per caller and per IP. Reads are not limited.
func (s *Server) limitWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && s.Limits != nil {
			if ok, wait := s.Limits.Allow(s.Limits.Writes, r, callerKey(principal(r))); !ok {
				ratelimit.RetryAfter(w, wait)
				writeError(w, http.StatusTooManyRequests, "too many requests; try again later")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// allowImport applies the import rate limit (creating and importing apps).
func (s *Server) allowImport(w http.ResponseWriter, r *http.Request) bool {
	if s.Limits == nil {
		return true
	}
	if ok, wait := s.Limits.Allow(s.Limits.Import, r, callerKey(principal(r))); !ok {
		ratelimit.RetryAfter(w, wait)
		writeError(w, http.StatusTooManyRequests, "too many new projects; try again later")
		return false
	}
	return true
}

// callerKey identifies a caller in rate limit buckets.
func callerKey(p auth.Principal) string {
	if p.UserID != 0 {
		return fmt.Sprint(p.UserID)
	}
	if p.Admin {
		return "admin"
	}
	return ""
}

// ---- webhook refusals ----

// refusePush answers a push the team's plan does not allow and marks the
// commit on GitHub, in the background (GitHub waits 10 s for the answer).
func (s *Server) refusePush(w http.ResponseWriter, app store.App, repo, sha, branch string, le *plans.LimitError) {
	s.Log.Info("deployment refused", "app", app.Name, "branch", branch, "sha", sha, "limit", le.Limit, "plan", le.Plan)
	if s.Refusals != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := s.Refusals.DeploymentRefused(ctx, repo, sha, le.Msg); err != nil {
				s.Log.Warn("github: refusal status", "app", app.Name, "sha", sha, "err", err)
			}
		}()
	}
	writeJSON(w, le.Status, map[string]any{"result": "refused", "reason": le.Msg, "limit": le.Limit,
		"plan": le.Plan, "max": le.Max, "used": le.Used})
}

// ---- usage ----

func (s *Server) teamUsage(w http.ResponseWriter, r *http.Request) {
	t, err := s.Auth.Team(r.Context(), principal(r), r.PathValue("slug"))
	if s.teamError(w, err, "team") {
		return
	}
	st, err := PlansFor(s.Plans, s.Store).Status(r.Context(), t.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	st.Role = t.Role
	writeJSON(w, http.StatusOK, st)
}

// ---- admin ----

func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Auth.PlatformAdmin(principal(r)) {
			writeError(w, http.StatusForbidden, "this endpoint needs a platform admin (PAAS_ADMIN_GITHUB_LOGINS or the admin token)")
			return
		}
		h(w, r)
	}
}

// AdminTeams lists every team with its plan, limits and usage.
func AdminTeams(ctx context.Context, st *store.Store, g *plans.Guard) ([]AdminTeamView, error) {
	g = PlansFor(g, st)
	month, day := g.Windows()
	rows, err := st.TeamsWithUsage(ctx, month, day)
	if err != nil {
		return nil, err
	}
	out := make([]AdminTeamView, 0, len(rows))
	for _, row := range rows {
		out = append(out, AdminTeamView{TeamStatus: g.StatusOf(row), Members: row.Members, Owners: row.Owners})
	}
	return out, nil
}

// AdminTeamView is a team in the admin list.
type AdminTeamView struct {
	plans.TeamStatus
	Members int      `json:"members"`
	Owners  []string `json:"owners"`
}

func (s *Server) adminTeams(w http.ResponseWriter, r *http.Request) {
	list, err := AdminTeams(r.Context(), s.Store, s.Plans)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) adminTeam(w http.ResponseWriter, r *http.Request) {
	t, ok := s.adminLookupTeam(w, r)
	if !ok {
		return
	}
	st, err := PlansFor(s.Plans, s.Store).Status(r.Context(), t.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) adminLookupTeam(w http.ResponseWriter, r *http.Request) (store.Team, bool) {
	t, err := s.Store.GetTeamBySlug(r.Context(), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "team not found")
		return t, false
	}
	if err != nil {
		s.internalError(w, err)
		return t, false
	}
	return t, true
}

// Rejected admin input is an *InputError (comments.go); its message is
// meant for the user.

// SetTeamPlan changes a team's plan. limits replaces the team's overrides
// when set (an empty map clears them); nil keeps them. Errors:
// *InputError, store errors.
func SetTeamPlan(ctx context.Context, st *store.Store, team store.Team, plan string, limits map[string]any) error {
	plan = strings.ToLower(strings.TrimSpace(plan))
	if !store.ValidPlan(plan) {
		return &InputError{`plan must be "hobby", "pro" or "unlimited"`}
	}
	var overrides json.RawMessage
	if limits == nil {
		cur, err := st.GetTeamPlan(ctx, team.ID)
		if err != nil {
			return err
		}
		overrides = cur.Overrides
	} else {
		var err error
		if overrides, err = plans.ValidateOverrides(limits); err != nil {
			return &InputError{err.Error()}
		}
	}
	return st.SetTeamPlan(ctx, team.ID, plan, overrides)
}

type planRequest struct {
	Plan string `json:"plan"`
	// Limits: absent keeps the overrides, null or {} clears them.
	Limits json.RawMessage `json:"limits"`
}

func (s *Server) adminSetPlan(w http.ResponseWriter, r *http.Request) {
	t, ok := s.adminLookupTeam(w, r)
	if !ok {
		return
	}
	var req planRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var limits map[string]any
	if len(req.Limits) > 0 {
		limits = map[string]any{}
		if string(req.Limits) != "null" {
			if err := json.Unmarshal(req.Limits, &limits); err != nil {
				writeError(w, http.StatusBadRequest, `limits must be an object such as {"projects": 50, "memory": "4Gi"}`)
				return
			}
		}
	}
	err := SetTeamPlan(r.Context(), s.Store, t, req.Plan, limits)
	var in *InputError
	if errors.As(err, &in) {
		writeError(w, http.StatusBadRequest, in.Msg)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("team plan changed", "team", t.Slug, "plan", req.Plan, "by", principal(r).Login)
	s.adminTeam(w, r)
}

// SuspendTeam suspends a team, stops its in-flight deployments and scales
// its apps to zero (cluster may be nil). clusterErr collects the apps that
// could not be scaled; the suspension is recorded regardless and quota
// keeps new pods out (deploy.Kubernetes), so it can be retried.
func SuspendTeam(ctx context.Context, st *store.Store, cluster Suspender, log *slog.Logger, team store.Team, reason, by string) (stopped int, clusterErr, err error) {
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		return 0, nil, &InputError{"reason must be at most 500 characters"}
	}
	if stopped, err = st.SuspendTeam(ctx, team.ID, reason); err != nil {
		return 0, nil, err
	}
	log.Info("team suspended", "team", team.Slug, "stopped", stopped, "by", by)
	return stopped, eachApp(ctx, st, team, func(ctx context.Context, app string) error {
		if cluster == nil {
			return nil
		}
		return cluster.SuspendApp(ctx, app)
	}), nil
}

// UnsuspendTeam lifts a suspension and scales the apps back.
func UnsuspendTeam(ctx context.Context, st *store.Store, cluster Suspender, log *slog.Logger, team store.Team, by string) (clusterErr, err error) {
	if err := st.UnsuspendTeam(ctx, team.ID); err != nil {
		return nil, err
	}
	log.Info("team unsuspended", "team", team.Slug, "by", by)
	return eachApp(ctx, st, team, func(ctx context.Context, app string) error {
		if cluster == nil {
			return nil
		}
		return cluster.ResumeApp(ctx, app)
	}), nil
}

func eachApp(ctx context.Context, st *store.Store, team store.Team, fn func(context.Context, string) error) error {
	apps, err := st.TeamAppNames(ctx, team.ID)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range apps {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := fn(cctx, a); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
		}
		cancel()
	}
	return errors.Join(errs...)
}

type suspendRequest struct {
	Reason string `json:"reason"`
}

type suspendView struct {
	plans.TeamStatus
	// Stopped: deployments canceled or asked to stop.
	Stopped int `json:"stopped"`
	// Warning: scaling some apps failed; repeat the request.
	Warning string `json:"warning,omitempty"`
}

func (s *Server) adminSuspend(w http.ResponseWriter, r *http.Request) {
	t, ok := s.adminLookupTeam(w, r)
	if !ok {
		return
	}
	var req suspendRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stopped, clusterErr, err := SuspendTeam(r.Context(), s.Store, s.Suspender, s.Log, t, req.Reason, principal(r).Login)
	s.suspendResult(w, r, t, stopped, clusterErr, err)
}

func (s *Server) adminUnsuspend(w http.ResponseWriter, r *http.Request) {
	t, ok := s.adminLookupTeam(w, r)
	if !ok {
		return
	}
	clusterErr, err := UnsuspendTeam(r.Context(), s.Store, s.Suspender, s.Log, t, principal(r).Login)
	s.suspendResult(w, r, t, 0, clusterErr, err)
}

func (s *Server) suspendResult(w http.ResponseWriter, r *http.Request, t store.Team, stopped int, clusterErr, err error) {
	var in *InputError
	if errors.As(err, &in) {
		writeError(w, http.StatusBadRequest, in.Msg)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	st, err := PlansFor(s.Plans, s.Store).Status(r.Context(), t.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	v := suspendView{TeamStatus: st, Stopped: stopped}
	if clusterErr != nil {
		s.Log.Error("suspension: cluster", "team", t.Slug, "err", clusterErr)
		v.Warning = "scaling some apps failed (repeat the request): " + clusterErr.Error()
	}
	writeJSON(w, http.StatusOK, v)
}

// SuspendUser suspends the user with a GitHub login; platform admins
// cannot be suspended.
func SuspendUser(ctx context.Context, st *store.Store, a *auth.Authenticator, login, reason string) (store.User, error) {
	u, err := st.UserByLogin(ctx, strings.TrimPrefix(strings.TrimSpace(login), "@"))
	if err != nil {
		return u, err
	}
	if a.PlatformAdmin(auth.Principal{UserID: u.ID, Login: u.Login}) {
		return u, &InputError{"platform admins cannot be suspended"}
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		return u, &InputError{"reason must be at most 500 characters"}
	}
	return u, st.SuspendUser(ctx, u.ID, reason)
}

// UnsuspendUser lifts a user's suspension.
func UnsuspendUser(ctx context.Context, st *store.Store, login string) (store.User, error) {
	u, err := st.UserByLogin(ctx, strings.TrimPrefix(strings.TrimSpace(login), "@"))
	if err != nil {
		return u, err
	}
	return u, st.UnsuspendUser(ctx, u.ID)
}

func (s *Server) adminSuspendedUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.SuspendedUsers(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) adminSuspendUser(w http.ResponseWriter, r *http.Request) {
	var req suspendRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := SuspendUser(r.Context(), s.Store, s.Auth, r.PathValue("login"), req.Reason)
	s.userResult(w, r, u, err, "user suspended")
}

func (s *Server) adminUnsuspendUser(w http.ResponseWriter, r *http.Request) {
	u, err := UnsuspendUser(r.Context(), s.Store, r.PathValue("login"))
	s.userResult(w, r, u, err, "user unsuspended")
}

func (s *Server) userResult(w http.ResponseWriter, r *http.Request, u store.User, err error, what string) {
	var in *InputError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "user not found")
		return
	case errors.As(err, &in):
		writeError(w, http.StatusConflict, in.Msg)
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	s.Log.Info(what, "user", u.Login, "by", principal(r).Login)
	suspended, err := s.Store.UserSuspended(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"login": u.Login, "suspended": suspended})
}
