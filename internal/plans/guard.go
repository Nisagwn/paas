package plans

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
)

// Limit names in LimitError.Limit (the JSON override keys).
const (
	LimitProjects     = "projects"
	LimitBuildMinutes = "build_minutes"
	LimitDeployments  = "deployments_per_day"
	LimitDomains      = "domains"
	LimitAddons       = "addons"
	LimitReplicas     = "replicas"
	LimitCrons        = "crons"
	// LimitSuspended: the team is suspended (not a counter).
	LimitSuspended = "suspended"
)

// LimitError refuses an action because of the team's plan. Its message is
// English (the JSON API); the web UI translates it (internal/web).
type LimitError struct {
	Limit string `json:"limit"`
	Plan  string `json:"plan"`
	Max   int    `json:"max"`
	Used  int64  `json:"used"`
	// Status is the HTTP status to answer with: 429 for the windowed
	// limits (they reset), 403 otherwise.
	Status int    `json:"-"`
	Msg    string `json:"error"`
}

func (e *LimitError) Error() string { return e.Msg }

// AsLimit returns the *LimitError in err's chain, or nil.
func AsLimit(err error) *LimitError {
	var le *LimitError
	if errors.As(err, &le) {
		return le
	}
	return nil
}

// Guard checks actions against the team's plan. A nil Guard allows all.
type Guard struct {
	Store   *store.Store
	Catalog Catalog
	// Now is the clock of the usage windows (tests); nil is time.Now.
	Now func() time.Time
}

func (g *Guard) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

// Windows returns the start of the current month and day (UTC).
func (g *Guard) Windows() (month, day time.Time) {
	n := g.now()
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC),
		time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// TeamStatus is a team's plan, effective limits and usage.
type TeamStatus struct {
	store.TeamPlan
	Limits Limits          `json:"limits"`
	Usage  store.TeamUsage `json:"usage"`
	// BuildMinutes is Usage.BuildSeconds rounded up to minutes.
	BuildMinutes int64 `json:"build_minutes"`
	// MonthStart and DayStart bound the windows of the usage.
	MonthStart time.Time `json:"month_start"`
	DayStart   time.Time `json:"day_start"`
}

// Status loads team id's plan, limits and usage.
func (g *Guard) Status(ctx context.Context, teamID int64) (TeamStatus, error) {
	tp, err := g.Store.GetTeamPlan(ctx, teamID)
	if err != nil {
		return TeamStatus{}, err
	}
	return g.status(ctx, tp)
}

func (g *Guard) status(ctx context.Context, tp store.TeamPlan) (TeamStatus, error) {
	month, day := g.Windows()
	u, err := g.Store.TeamUsage(ctx, tp.ID, month, day)
	if err != nil {
		return TeamStatus{}, err
	}
	return TeamStatus{TeamPlan: tp, Limits: g.limits(tp), Usage: u, BuildMinutes: minutes(u.BuildSeconds),
		MonthStart: month, DayStart: day}, nil
}

// StatusOf builds the status of an already loaded row (admin list).
func (g *Guard) StatusOf(row store.TeamWithUsage) TeamStatus {
	month, day := g.Windows()
	return TeamStatus{TeamPlan: row.TeamPlan, Limits: g.limits(row.TeamPlan), Usage: row.Usage,
		BuildMinutes: minutes(row.Usage.BuildSeconds), MonthStart: month, DayStart: day}
}

func minutes(seconds int64) int64 { return (seconds + 59) / 60 }

func (g *Guard) limits(tp store.TeamPlan) Limits {
	c := g.Catalog
	if c == nil {
		c = DefaultCatalog()
	}
	return c.Resolve(tp.Plan, tp.Overrides)
}

// Limits returns the effective limits of team id.
func (g *Guard) Limits(ctx context.Context, teamID int64) (Limits, store.TeamPlan, error) {
	tp, err := g.Store.GetTeamPlan(ctx, teamID)
	if err != nil {
		return Limits{}, tp, err
	}
	return g.limits(tp), tp, nil
}

func suspended(tp store.TeamPlan) error {
	msg := "the team is suspended: deployments and new resources are disabled"
	if tp.SuspendedReason != "" {
		msg += " (" + tp.SuspendedReason + ")"
	}
	return &LimitError{Limit: LimitSuspended, Plan: tp.Plan, Status: http.StatusForbidden, Msg: msg}
}

func over(limit string, tp store.TeamPlan, max int, used int64, status int, msg string) error {
	return &LimitError{Limit: limit, Plan: tp.Plan, Max: max, Used: used, Status: status, Msg: msg}
}

// reached reports whether used has reached max (Unlimited never does).
func reached(max int, used int64) bool { return max >= 0 && used >= int64(max) }

// check loads the team and runs fn on its status; a suspended team fails.
func (g *Guard) check(ctx context.Context, teamID int64, fn func(TeamStatus) error) error {
	if g == nil {
		return nil
	}
	st, err := g.Status(ctx, teamID)
	if err != nil {
		return err
	}
	if st.Suspended() {
		return suspended(st.TeamPlan)
	}
	return fn(st)
}

// CheckCreateApp: one more project for team id.
func (g *Guard) CheckCreateApp(ctx context.Context, teamID int64) error {
	return g.check(ctx, teamID, func(st TeamStatus) error {
		if reached(st.Limits.Projects, int64(st.Usage.Projects)) {
			return over(LimitProjects, st.TeamPlan, st.Limits.Projects, int64(st.Usage.Projects), http.StatusForbidden,
				fmt.Sprintf("plan limit reached: the %s plan allows %d projects per team", Title(st.Plan), st.Limits.Projects))
		}
		return nil
	})
}

// CheckDeploy: one more deployment for team id (daily count and monthly
// build minutes).
func (g *Guard) CheckDeploy(ctx context.Context, teamID int64) error {
	return g.check(ctx, teamID, func(st TeamStatus) error {
		if reached(st.Limits.DeploymentsPerDay, int64(st.Usage.DeploymentsToday)) {
			return over(LimitDeployments, st.TeamPlan, st.Limits.DeploymentsPerDay, int64(st.Usage.DeploymentsToday),
				http.StatusTooManyRequests,
				fmt.Sprintf("plan limit reached: the %s plan allows %d deployments per day", Title(st.Plan), st.Limits.DeploymentsPerDay))
		}
		if reached(st.Limits.BuildMinutes, st.BuildMinutes) {
			return over(LimitBuildMinutes, st.TeamPlan, st.Limits.BuildMinutes, st.BuildMinutes, http.StatusTooManyRequests,
				fmt.Sprintf("plan limit reached: the %s plan allows %d build minutes per month", Title(st.Plan), st.Limits.BuildMinutes))
		}
		return nil
	})
}

// CheckDeployApp is CheckDeploy for the team that owns app id.
func (g *Guard) CheckDeployApp(ctx context.Context, appID int64) error {
	if g == nil {
		return nil
	}
	tp, err := g.Store.TeamPlanOfApp(ctx, appID)
	if err != nil {
		return err
	}
	return g.CheckDeploy(ctx, tp.ID)
}

// CheckDomain: one more custom domain for team id.
func (g *Guard) CheckDomain(ctx context.Context, teamID int64) error {
	return g.check(ctx, teamID, func(st TeamStatus) error {
		if reached(st.Limits.Domains, int64(st.Usage.Domains)) {
			return over(LimitDomains, st.TeamPlan, st.Limits.Domains, int64(st.Usage.Domains), http.StatusForbidden,
				fmt.Sprintf("plan limit reached: the %s plan allows %d custom domains per team", Title(st.Plan), st.Limits.Domains))
		}
		return nil
	})
}

// CheckAddon: one more add-on (managed database) for team id.
func (g *Guard) CheckAddon(ctx context.Context, teamID int64) error {
	return g.check(ctx, teamID, func(st TeamStatus) error {
		if reached(st.Limits.Addons, int64(st.Usage.Addons)) {
			return over(LimitAddons, st.TeamPlan, st.Limits.Addons, int64(st.Usage.Addons), http.StatusForbidden,
				fmt.Sprintf("plan limit reached: the %s plan allows %d databases per team", Title(st.Plan), st.Limits.Addons))
		}
		return nil
	})
}

// CheckReplicas: a worker process of team id running n replicas.
func (g *Guard) CheckReplicas(ctx context.Context, teamID int64, n int) error {
	return g.check(ctx, teamID, func(st TeamStatus) error {
		return replicasOver(st.TeamPlan, st.Limits, "", n)
	})
}

func replicasOver(tp store.TeamPlan, l Limits, proc string, n int) error {
	if l.Replicas < 0 || n <= l.Replicas {
		return nil
	}
	msg := fmt.Sprintf("plan limit reached: the %s plan allows %d replicas per process", Title(tp.Plan), l.Replicas)
	if proc != "" {
		msg += fmt.Sprintf("; paas.yaml asks for %d replicas of %s", n, proc)
	}
	return over(LimitReplicas, tp, l.Replicas, int64(n), http.StatusForbidden, msg)
}

// Title is the display name of a plan: Hobby, Pro, Unlimited.
func Title(plan string) string {
	switch plan {
	case store.PlanHobby:
		return "Hobby"
	case store.PlanPro:
		return "Pro"
	case store.PlanUnlimited:
		return "Unlimited"
	}
	return plan
}

// CheckProcesses checks a built process set (paas.yaml) of a deployment of
// app id: worker replicas and cron jobs. The worker fails the deployment
// with the error.
func (g *Guard) CheckProcesses(ctx context.Context, appID int64, set process.Set) error {
	if g == nil {
		return nil
	}
	tp, err := g.Store.TeamPlanOfApp(ctx, appID)
	if err != nil {
		return err
	}
	l := g.limits(tp)
	if l.Crons >= 0 && len(set.Crons) > l.Crons {
		return over(LimitCrons, tp, l.Crons, int64(len(set.Crons)), http.StatusForbidden,
			fmt.Sprintf("plan limit reached: the %s plan allows %d cron jobs; paas.yaml declares %d", Title(tp.Plan), l.Crons, len(set.Crons)))
	}
	for _, w := range set.Workers {
		if err := replicasOver(tp, l, w.Name, w.Replicas); err != nil {
			return err
		}
	}
	return nil
}

// BuildTimeout is the build timeout of the team that owns app id; 0 means
// none.
func (g *Guard) BuildTimeout(ctx context.Context, appID int64) (time.Duration, error) {
	if g == nil {
		return 0, nil
	}
	tp, err := g.Store.TeamPlanOfApp(ctx, appID)
	if err != nil {
		return 0, err
	}
	return g.limits(tp).Timeout(), nil
}

// ClaimCaps are the per-plan bounds of concurrent builds for the queue.
func (g *Guard) ClaimCaps() store.ClaimCaps {
	if g == nil {
		return store.ClaimCaps{}
	}
	c := g.Catalog
	if c == nil {
		c = DefaultCatalog()
	}
	caps := store.ClaimCaps{ByPlan: map[string]int{}}
	for p, l := range c {
		caps.ByPlan[p] = l.ConcurrentBuilds
	}
	return caps
}

// AppQuota is the namespace quota of an app (deploy.PlanQuotas): the
// plan's CPU and memory ("" = the global quota) and whether its team is
// suspended (no pods may run).
func (g *Guard) AppQuota(ctx context.Context, app string) (cpu, memory string, isSuspended bool, err error) {
	if g == nil {
		return "", "", false, nil
	}
	tp, err := g.Store.TeamPlanOfAppName(ctx, app)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	l := g.limits(tp)
	return l.CPU, l.Memory, tp.Suspended(), nil
}
