package plans_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func sha(n int) string { return fmt.Sprintf("%040x", n) }

// tight is a catalog whose hobby plan allows one of everything.
func tight() plans.Catalog {
	c := plans.DefaultCatalog()
	h := c[store.PlanHobby]
	h.Projects, h.Domains, h.Addons, h.Replicas, h.Crons = 1, 1, 1, 2, 1
	h.DeploymentsPerDay, h.BuildMinutes, h.BuildTimeout, h.ConcurrentBuilds = 2, 1, 7, 1
	h.CPU, h.Memory = "500m", "1Gi"
	c[store.PlanHobby] = h
	return c
}

func expectLimit(t *testing.T, err error, limit string, status int) *plans.LimitError {
	t.Helper()
	le := plans.AsLimit(err)
	if le == nil {
		t.Fatalf("want %s refusal, got %v", limit, err)
	}
	if le.Limit != limit || le.Status != status || !strings.HasPrefix(le.Msg, "plan limit reached") && limit != plans.LimitSuspended {
		t.Fatalf("refusal %+v, want %s/%d", le, limit, status)
	}
	return le
}

func TestGuardChecks(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	g := &plans.Guard{Store: st, Catalog: tight()}
	team, _ := st.CreateTeam(ctx, "web", "Web", 0)

	// Under every limit.
	for name, err := range map[string]error{"create": g.CheckCreateApp(ctx, team.ID), "deploy": g.CheckDeploy(ctx, team.ID),
		"domain": g.CheckDomain(ctx, team.ID), "addon": g.CheckAddon(ctx, team.ID), "replicas": g.CheckReplicas(ctx, team.ID, 2)} {
		if err != nil {
			t.Fatalf("%s under the limit: %v", name, err)
		}
	}
	app, _ := st.CreateAppInTeam(ctx, team.ID, "blog", "o/blog", "main")
	le := expectLimit(t, g.CheckCreateApp(ctx, team.ID), plans.LimitProjects, http.StatusForbidden)
	if le.Max != 1 || le.Used != 1 || le.Plan != store.PlanHobby ||
		le.Msg != "plan limit reached: the Hobby plan allows 1 projects per team" {
		t.Fatalf("projects refusal %+v", le)
	}
	st.AddDomain(ctx, app.ID, "www.example.com", "tok")
	expectLimit(t, g.CheckDomain(ctx, team.ID), plans.LimitDomains, http.StatusForbidden)
	st.CreateAddon(ctx, app.ID, store.NewAddon{Kind: "redis", Name: "cache", Plan: "hobby", PreviewMode: "shared"})
	expectLimit(t, g.CheckAddon(ctx, team.ID), plans.LimitAddons, http.StatusForbidden)
	expectLimit(t, g.CheckReplicas(ctx, team.ID, 3), plans.LimitReplicas, http.StatusForbidden)

	d1, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "")
	if err := g.CheckDeploy(ctx, team.ID); err != nil {
		t.Fatalf("one deployment today: %v", err)
	}
	st.EnqueueDeployment(ctx, app.ID, sha(2), "main", "")
	expectLimit(t, g.CheckDeploy(ctx, team.ID), plans.LimitDeployments, http.StatusTooManyRequests)
	if err := g.CheckDeployApp(ctx, app.ID); plans.AsLimit(err) == nil {
		t.Fatalf("CheckDeployApp: %v", err)
	}

	// Tomorrow the daily count resets, but the month's build minute is used.
	st.AddBuildSeconds(ctx, d1.ID, 61)
	g.Now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if month, _ := g.Windows(); month.Month() == time.Now().UTC().Month() {
		expectLimit(t, g.CheckDeploy(ctx, team.ID), plans.LimitBuildMinutes, http.StatusTooManyRequests)
	}
	g.Now = nil

	// An admin override lifts single limits.
	st.SetTeamPlan(ctx, team.ID, store.PlanHobby, json.RawMessage(`{"projects": 5, "deployments_per_day": -1, "build_minutes": -1}`))
	if err := g.CheckCreateApp(ctx, team.ID); err != nil {
		t.Fatalf("override: %v", err)
	}
	if err := g.CheckDeploy(ctx, team.ID); err != nil {
		t.Fatalf("override: %v", err)
	}

	// The usage status reports limits and counters.
	s, err := g.Status(ctx, team.ID)
	if err != nil || s.Limits.Projects != 5 || s.Usage.Projects != 1 || s.Usage.DeploymentsToday != 2 || s.BuildMinutes != 2 {
		t.Fatalf("status %+v %v", s, err)
	}

	// A suspended team fails every check, even unlimited ones.
	st.SetTeamPlan(ctx, team.ID, store.PlanUnlimited, nil)
	st.SuspendTeam(ctx, team.ID, "spam")
	for _, err := range []error{g.CheckCreateApp(ctx, team.ID), g.CheckDeploy(ctx, team.ID), g.CheckDomain(ctx, team.ID),
		g.CheckAddon(ctx, team.ID), g.CheckReplicas(ctx, team.ID, 1)} {
		le := expectLimit(t, err, plans.LimitSuspended, http.StatusForbidden)
		if !strings.Contains(le.Msg, "(spam)") {
			t.Fatalf("suspension message %q", le.Msg)
		}
	}
}

func TestGuardWorkerSide(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	g := &plans.Guard{Store: st, Catalog: tight()}
	team, _ := st.CreateTeam(ctx, "web", "Web", 0)
	app, _ := st.CreateAppInTeam(ctx, team.ID, "blog", "o/blog", "main")

	if d, err := g.BuildTimeout(ctx, app.ID); err != nil || d != 7*time.Minute {
		t.Fatalf("timeout %s %v", d, err)
	}
	caps := g.ClaimCaps()
	if caps.ByPlan[store.PlanHobby] != 1 || caps.ByPlan[store.PlanUnlimited] != plans.Unlimited {
		t.Fatalf("caps %+v", caps)
	}

	ok := process.Set{Workers: []process.Worker{{Name: "queue", Replicas: 2}}, Crons: []process.Cron{{Name: "a"}}}
	if err := g.CheckProcesses(ctx, app.ID, ok); err != nil {
		t.Fatalf("within limits: %v", err)
	}
	crons := process.Set{Crons: []process.Cron{{Name: "a"}, {Name: "b"}}}
	le := expectLimit(t, g.CheckProcesses(ctx, app.ID, crons), plans.LimitCrons, http.StatusForbidden)
	if le.Msg != "plan limit reached: the Hobby plan allows 1 cron jobs; paas.yaml declares 2" {
		t.Fatalf("crons %q", le.Msg)
	}
	many := process.Set{Workers: []process.Worker{{Name: "queue", Replicas: 5}}}
	le = expectLimit(t, g.CheckProcesses(ctx, app.ID, many), plans.LimitReplicas, http.StatusForbidden)
	if !strings.HasSuffix(le.Msg, "paas.yaml asks for 5 replicas of queue") {
		t.Fatalf("replicas %q", le.Msg)
	}

	// The namespace quota follows the plan; unknown apps use the defaults.
	cpu, mem, suspended, err := g.AppQuota(ctx, "blog")
	if err != nil || cpu != "500m" || mem != "1Gi" || suspended {
		t.Fatalf("quota %s %s %v %v", cpu, mem, suspended, err)
	}
	st.SuspendTeam(ctx, team.ID, "")
	if _, _, suspended, _ := g.AppQuota(ctx, "blog"); !suspended {
		t.Fatal("suspension not in quota")
	}
	if cpu, mem, suspended, err := g.AppQuota(ctx, "nope"); cpu != "" || mem != "" || suspended || err != nil {
		t.Fatalf("unknown app: %s %s %v %v", cpu, mem, suspended, err)
	}
}
