package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func planSHA(n int) string { return fmt.Sprintf("%040x", n) }

// Teams start on hobby; the default team (created before migration 016)
// is unlimited. Plans and overrides round-trip; bad plans are refused.
func TestTeamPlans(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	def, err := st.GetTeamBySlug(ctx, store.DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	if tp, _ := st.GetTeamPlan(ctx, def.ID); tp.Plan != store.PlanUnlimited {
		t.Fatalf("default team plan %q", tp.Plan)
	}
	team, _ := st.CreateTeam(ctx, "web", "Web", 0)
	tp, err := st.GetTeamPlan(ctx, team.ID)
	if err != nil || tp.Plan != store.PlanHobby || string(tp.Overrides) != "{}" || tp.Suspended() {
		t.Fatalf("new team: %+v %v", tp, err)
	}
	if err := st.SetTeamPlan(ctx, team.ID, store.PlanPro, json.RawMessage(`{"projects": 50}`)); err != nil {
		t.Fatal(err)
	}
	app, _ := st.CreateAppInTeam(ctx, team.ID, "blog", "o/blog", "main")
	if tp, _ := st.TeamPlanOfApp(ctx, app.ID); tp.Plan != store.PlanPro || string(tp.Overrides) != `{"projects": 50}` {
		t.Fatalf("plan of app: %+v", tp)
	}
	if tp, _ := st.TeamPlanOfAppName(ctx, "blog"); tp.ID != team.ID {
		t.Fatalf("plan of app name: %+v", tp)
	}
	if err := st.SetTeamPlan(ctx, team.ID, "gold", nil); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("invalid plan: %v", err)
	}
	if err := st.SetTeamPlan(ctx, 999999, store.PlanPro, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown team: %v", err)
	}
	if err := st.SetTeamPlan(ctx, team.ID, store.PlanHobby, nil); err != nil {
		t.Fatal(err)
	}
	if tp, _ := st.GetTeamPlan(ctx, team.ID); string(tp.Overrides) != "{}" {
		t.Fatalf("overrides not cleared: %s", tp.Overrides)
	}
}

// Usage counts projects, domains, add-ons, today's deployments, this
// month's build time and running builds.
func TestTeamUsage(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	team, _ := st.CreateTeam(ctx, "web", "Web", 0)
	other, _ := st.CreateTeam(ctx, "ops", "Ops", 0)
	blog, _ := st.CreateAppInTeam(ctx, team.ID, "blog", "o/blog", "main")
	st.CreateAppInTeam(ctx, team.ID, "shop", "o/shop", "main")
	infra, _ := st.CreateAppInTeam(ctx, other.ID, "infra", "o/infra", "main")
	st.AddDomain(ctx, blog.ID, "www.example.com", "tok")
	if _, err := st.CreateAddon(ctx, blog.ID, store.NewAddon{Kind: "redis", Name: "cache", Plan: "hobby", PreviewMode: "shared"}); err != nil {
		t.Fatal(err)
	}
	d1, _, _ := st.EnqueueDeployment(ctx, blog.ID, planSHA(1), "main", "")
	d2, _, _ := st.EnqueueDeployment(ctx, blog.ID, planSHA(2), "main", "")
	st.EnqueueDeployment(ctx, infra.ID, planSHA(3), "main", "")
	st.AddBuildSeconds(ctx, d1.ID, 90)
	st.AddBuildSeconds(ctx, d1.ID, 30) // a second attempt adds
	st.AddBuildSeconds(ctx, d2.ID, -5) // never negative
	if n, _ := st.BuildSeconds(ctx, d1.ID); n != 120 {
		t.Fatalf("build seconds %d", n)
	}
	st.SetStatus(ctx, d2.ID, store.StatusBuilding)

	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	u, err := st.TeamUsage(ctx, team.ID, month, day)
	if err != nil {
		t.Fatal(err)
	}
	want := store.TeamUsage{Projects: 2, Domains: 1, Addons: 1, DeploymentsToday: 2, BuildSeconds: 120, ActiveBuilds: 1}
	if u != want {
		t.Fatalf("usage %+v, want %+v", u, want)
	}
	// Windows that start after the deployments count nothing.
	if u, _ := st.TeamUsage(ctx, team.ID, now.Add(time.Hour), now.Add(time.Hour)); u.DeploymentsToday != 0 || u.BuildSeconds != 0 {
		t.Fatalf("future window: %+v", u)
	}
	rows, err := st.TeamsWithUsage(ctx, month, day)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Slug == "web" {
			found = true
			if r.Usage != want || r.Members != 0 || len(r.Owners) != 0 {
				t.Fatalf("admin row: %+v", r)
			}
		}
	}
	if !found || len(rows) != 3 { // default, web, ops
		t.Fatalf("admin rows: %+v", rows)
	}
}

// The fair queue: a team at its bound of concurrent builds is skipped, the
// others go ahead; suspended teams are never claimed.
func TestClaimNextForFairness(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	a, _ := st.CreateTeam(ctx, "team-a", "A", 0)
	b, _ := st.CreateTeam(ctx, "team-b", "B", 0)
	c, _ := st.CreateTeam(ctx, "team-c", "C", 0)
	appA, _ := st.CreateAppInTeam(ctx, a.ID, "a1", "o/a1", "main")
	appB, _ := st.CreateAppInTeam(ctx, b.ID, "b1", "o/b1", "main")
	appC, _ := st.CreateAppInTeam(ctx, c.ID, "c1", "o/c1", "main")
	a1, _, _ := st.EnqueueDeployment(ctx, appA.ID, planSHA(1), "main", "")
	a2, _, _ := st.EnqueueDeployment(ctx, appA.ID, planSHA(2), "main", "")
	a3, _, _ := st.EnqueueDeployment(ctx, appA.ID, planSHA(3), "main", "")
	b1, _, _ := st.EnqueueDeployment(ctx, appB.ID, planSHA(4), "main", "")
	c1, _, _ := st.EnqueueDeployment(ctx, appC.ID, planSHA(5), "main", "")
	if _, err := st.SuspendTeam(ctx, c.ID, "abuse"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(ctx, c1.ID); got.Status != store.StatusCanceled || got.Error != store.SuspendedReason {
		t.Fatalf("suspended team's queued deployment: %+v", got)
	}

	caps := store.ClaimCaps{ByPlan: map[string]int{store.PlanHobby: 1, store.PlanPro: 2, store.PlanUnlimited: -1}}
	claim := func() int64 {
		t.Helper()
		d, err := st.ClaimNextFor(ctx, caps)
		if errors.Is(err, store.ErrNotFound) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	if got := claim(); got != a1.ID {
		t.Fatalf("first claim %d, want a1 %d", got, a1.ID)
	}
	// a is at its bound (hobby: 1): b goes ahead of a's older deployments.
	if got := claim(); got != b1.ID {
		t.Fatalf("second claim %d, want b1 %d", got, b1.ID)
	}
	if got := claim(); got != 0 {
		t.Fatalf("third claim %d, want none", got)
	}
	// A finished build frees the slot.
	st.MarkFailed(ctx, a1.ID, "x")
	if got := claim(); got != a2.ID {
		t.Fatalf("after a1 finished: %d, want a2 %d", got, a2.ID)
	}
	// The team's override wins over its plan's bound.
	st.SetTeamPlan(ctx, a.ID, store.PlanHobby, json.RawMessage(`{"concurrent_builds": 2}`))
	if got := claim(); got != a3.ID {
		t.Fatalf("with override: %d, want a3 %d", got, a3.ID)
	}

	// Unbounded claims (no plans configured) still skip suspended teams.
	c2, _, _ := st.EnqueueDeployment(ctx, appC.ID, planSHA(6), "main", "")
	if _, err := st.ClaimNext(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claimed a suspended team's deployment: %v", err)
	}
	if err := st.UnsuspendTeam(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if d, err := st.ClaimNext(ctx); err != nil || d.ID != c2.ID {
		t.Fatalf("after unsuspend: %+v %v", d, err)
	}
}

// Suspending a team stops its running deployments too; suspending users
// is recorded and listed.
func TestSuspension(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	team, _ := st.CreateTeam(ctx, "web", "Web", 0)
	app, _ := st.CreateAppInTeam(ctx, team.ID, "blog", "o/blog", "main")
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, planSHA(1), "main", "")
	st.SetStatus(ctx, d.ID, store.StatusBuilding)
	n, err := st.SuspendTeam(ctx, team.ID, "spam")
	if err != nil || n != 1 {
		t.Fatalf("suspend: %d %v", n, err)
	}
	if got, _ := st.GetDeployment(ctx, d.ID); got.Status != store.StatusBuilding || got.CancelRequestedAt == nil {
		t.Fatalf("running deployment: %+v", got)
	}
	tp, _ := st.GetTeamPlan(ctx, team.ID)
	if !tp.Suspended() || tp.SuspendedReason != "spam" {
		t.Fatalf("team: %+v", tp)
	}
	if names, _ := st.TeamAppNames(ctx, team.ID); len(names) != 1 || names[0] != "blog" {
		t.Fatalf("app names %v", names)
	}
	if _, err := st.SuspendTeam(ctx, 999999, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown team: %v", err)
	}

	u, _ := st.UpsertUser(ctx, 7, "mallory", "", "")
	if s, _ := st.UserSuspended(ctx, u.ID); s {
		t.Fatal("suspended before")
	}
	if err := st.SuspendUser(ctx, u.ID, "bots"); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.UserSuspended(ctx, u.ID); !s {
		t.Fatal("not suspended")
	}
	list, _ := st.SuspendedUsers(ctx)
	if len(list) != 1 || list[0].Login != "mallory" || list[0].SuspendedReason != "bots" {
		t.Fatalf("suspended users: %+v", list)
	}
	st.UnsuspendUser(ctx, u.ID)
	if s, _ := st.UserSuspended(ctx, u.ID); s {
		t.Fatal("still suspended")
	}
	if err := st.SuspendUser(ctx, 999999, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}
