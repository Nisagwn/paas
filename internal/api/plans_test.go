package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/ratelimit"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/webhook"
)

// fakeRefusals records the commit statuses of refused pushes.
type fakeRefusals struct {
	mu  sync.Mutex
	got []string
}

func (f *fakeRefusals) DeploymentRefused(_ context.Context, repo, sha, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, repo+"@"+sha[:7]+": "+reason)
	return nil
}

func (f *fakeRefusals) wait(t *testing.T, n int) []string {
	t.Helper()
	for i := 0; i < 200; i++ {
		f.mu.Lock()
		got := append([]string(nil), f.got...)
		f.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d refusal statuses", n)
	return nil
}

// fakeSuspender records the apps scaled down and back up.
type fakeSuspender struct {
	mu                 sync.Mutex
	suspended, resumed []string
}

func (f *fakeSuspender) SuspendApp(_ context.Context, app string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspended = append(f.suspended, app)
	return nil
}

func (f *fakeSuspender) ResumeApp(_ context.Context, app string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, app)
	return nil
}

// planCatalog: hobby allows 2 projects, 1 domain, 1 add-on, 2 replicas and
// 10 build minutes; deployments per day are set per test with overrides.
func planCatalog() plans.Catalog {
	c := plans.DefaultCatalog()
	h := c[store.PlanHobby]
	h.Projects, h.Domains, h.Addons, h.Replicas, h.BuildMinutes = 2, 1, 1, 2, 10
	c[store.PlanHobby] = h
	return c
}

// planServer points f at a server that enforces the plans; erin is a
// platform admin (PAAS_ADMIN_GITHUB_LOGINS).
func (f *teamFixture) planServer(edit func(*api.Server)) (*fakeRefusals, *fakeSuspender) {
	f.t.Helper()
	ref, sus := &fakeRefusals{}, &fakeSuspender{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &api.Server{
		Store: f.st, Domain: domain, APIToken: token, WebhookSecret: secret, Sessions: f.ss, Log: log,
		Auth:      &auth.Authenticator{Store: f.st, Sessions: f.ss, Log: log, Admins: []string{"erin"}},
		Plans:     &plans.Guard{Store: f.st, Catalog: planCatalog()},
		Refusals:  ref,
		Suspender: sus,
		GitHubApp: &fakeRepos{heads: map[string]string{"nisagwn/blog@main": sha(77)}},
	}
	if edit != nil {
		edit(s)
	}
	srv := httptest.NewServer(s.Handler())
	f.t.Cleanup(srv.Close)
	f.srv = srv
	return ref, sus
}

// override sets the web team's per-team limits.
func (f *teamFixture) override(limits string) {
	f.t.Helper()
	if err := f.st.SetTeamPlan(context.Background(), f.web.ID, store.PlanHobby, json.RawMessage(limits)); err != nil {
		f.t.Fatal(err)
	}
}

// pushTo sends a signed push webhook.
func (f *teamFixture) pushTo(repo, branch, commit string) (int, string) {
	f.t.Helper()
	body := []byte(fmt.Sprintf(`{"ref":"refs/heads/%s","after":"%s","repository":{"full_name":"%s"},
		"head_commit":{"message":"commit"}}`, branch, commit, repo))
	req, _ := http.NewRequest("POST", f.srv.URL+"/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", webhook.Sign(secret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func wantLimit(t *testing.T, what string, code int, body string, status int, limit string) {
	t.Helper()
	var le struct {
		Error  string `json:"error"`
		Reason string `json:"reason"` // the webhook's answer
		Limit  string `json:"limit"`
		Plan   string `json:"plan"`
	}
	json.Unmarshal([]byte(body), &le)
	if code != status || le.Limit != limit || le.Plan != store.PlanHobby || le.Error+le.Reason == "" {
		t.Fatalf("%s: %d %s, want %d limit %s", what, code, body, status, limit)
	}
}

// Every entry point that creates something checks the team's plan.
func TestPlanLimitsAPI(t *testing.T) {
	f := setupTeams(t)
	ref, _ := f.planServer(nil)
	ctx := context.Background()

	// Projects: web has blog; one more fits.
	if code, b := f.call("bob", "POST", "/api/apps", map[string]string{"name": "shop", "repo": "nisagwn/shop", "team": "web"}); code != 201 {
		t.Fatalf("second project: %d %s", code, b)
	}
	code, body := f.call("bob", "POST", "/api/apps", map[string]string{"name": "extra", "repo": "nisagwn/extra", "team": "web"})
	wantLimit(t, "third project", code, body, 403, plans.LimitProjects)
	if !strings.Contains(body, `"max":2`) || !strings.Contains(body, `"used":2`) {
		t.Fatalf("limit details: %s", body)
	}
	f.claimInstallation(9, f.web.ID, "acme/imported")
	code, body = f.call("bob", "POST", "/api/apps/import", map[string]string{"repo": "acme/imported", "team": "web"})
	wantLimit(t, "import", code, body, 403, plans.LimitProjects)
	// Another team is not affected (ops: one project).
	if code, b := f.call("dave", "POST", "/api/apps", map[string]string{"name": "tool", "repo": "nisagwn/tool"}); code != 201 {
		t.Fatalf("other team: %d %s", code, b)
	}

	// Custom domains and add-ons.
	if code, b := f.call("bob", "POST", "/api/apps/blog/domains", map[string]string{"hostname": "www.one.example"}); code != 201 {
		t.Fatalf("first domain: %d %s", code, b)
	}
	code, body = f.call("bob", "POST", "/api/apps/blog/domains", map[string]string{"hostname": "www.two.example"})
	wantLimit(t, "second domain", code, body, 403, plans.LimitDomains)
	if code, b := f.call("bob", "POST", "/api/apps/blog/addons", map[string]string{"kind": "redis", "name": "cache"}); code != 201 {
		t.Fatalf("first add-on: %d %s", code, b)
	}
	code, body = f.call("bob", "POST", "/api/apps/blog/addons", map[string]string{"kind": "redis", "name": "cache2"})
	wantLimit(t, "second add-on", code, body, 403, plans.LimitAddons)

	// Worker replicas.
	code, body = f.call("bob", "PUT", "/api/apps/blog/processes/queue", map[string]int{"replicas": 3})
	wantLimit(t, "replicas", code, body, 403, plans.LimitReplicas)

	// Deployments per day: one more is allowed, then every way of
	// queueing one is refused.
	f.override(`{"deployments_per_day": 3}`) // d1 and d2 exist
	if code, b := f.call("bob", "POST", fmt.Sprintf("/api/apps/blog/deployments/%d/redeploy", f.d1.ID), nil); code != 202 {
		t.Fatalf("redeploy under the limit: %d %s", code, b)
	}
	code, body = f.call("bob", "POST", fmt.Sprintf("/api/apps/blog/deployments/%d/redeploy", f.d2.ID), nil)
	wantLimit(t, "redeploy", code, body, 429, plans.LimitDeployments)

	code, body = f.pushTo("nisagwn/blog", "main", sha(50))
	wantLimit(t, "push", code, body, 429, plans.LimitDeployments)
	if !strings.Contains(body, `"result":"refused"`) {
		t.Fatalf("push answer: %s", body)
	}
	got := ref.wait(t, 1)
	if !strings.HasPrefix(got[0], "nisagwn/blog@"+sha(50)[:7]+": plan limit reached") {
		t.Fatalf("GitHub status: %v", got)
	}
	blog, _ := f.st.GetAppByName(ctx, "blog")
	if list, _ := f.st.ListDeployments(ctx, blog.ID, 50); len(list) != 3 {
		t.Fatalf("refused push queued a deployment: %d", len(list))
	}

	// Promoting a preview queues a deployment.
	prev, _, _ := f.st.EnqueueDeployment(ctx, blog.ID, sha(60), "feature", "preview")
	f.st.MarkReady(ctx, prev, []store.AliasSpec{{Hostname: "feature-blog." + domain, Kind: store.AliasPreview, Branch: "feature"}})
	f.override(`{"deployments_per_day": 4}`)
	code, body = f.call("bob", "POST", "/api/apps/blog/promote", map[string]int64{"deployment_id": prev.ID})
	wantLimit(t, "promote preview", code, body, 429, plans.LimitDeployments)
	// Moving the production alias back (a rollback-like promote) is free.
	if code, b := f.call("bob", "POST", "/api/apps/blog/promote", map[string]int64{"deployment_id": f.d1.ID}); code != 200 {
		t.Fatalf("promote production: %d %s", code, b)
	}

	// Deploy hooks.
	var hook struct {
		Path string `json:"path"`
	}
	_, hb := f.call("bob", "POST", "/api/apps/blog/hooks", map[string]string{"name": "cms"})
	json.Unmarshal([]byte(hb), &hook)
	code, body = f.call("anonymous", "POST", hook.Path, nil)
	wantLimit(t, "deploy hook", code, body, 429, plans.LimitDeployments)

	// Build minutes this month.
	f.override(`{"deployments_per_day": "unlimited"}`)
	if code, b := f.call("anonymous", "POST", hook.Path, nil); code != 201 && code != 200 {
		t.Fatalf("hook under the limits: %d %s", code, b)
	}
	f.st.AddBuildSeconds(ctx, f.d1.ID, 10*60)
	code, body = f.pushTo("nisagwn/blog", "main", sha(51))
	wantLimit(t, "push over build minutes", code, body, 429, plans.LimitBuildMinutes)
	ref.wait(t, 2)

	// The usage endpoint shows it all.
	code, body = f.call("carol", "GET", "/api/teams/web/usage", nil)
	var u plans.TeamStatus
	json.Unmarshal([]byte(body), &u)
	if code != 200 || u.Plan != store.PlanHobby || u.Usage.Projects != 2 || u.Usage.Domains != 1 || u.Usage.Addons != 1 ||
		u.BuildMinutes != 10 || u.Limits.Projects != 2 || u.Limits.DeploymentsPerDay != plans.Unlimited {
		t.Fatalf("usage: %d %s", code, body)
	}
}

// Import queues no first deployment when the day's deployments are used
// up; the app is still created.
func TestImportOverDeployLimit(t *testing.T) {
	f := setupTeams(t)
	f.planServer(func(s *api.Server) {
		s.GitHubApp = &fakeRepos{heads: map[string]string{"acme/site@main": sha(9)}, defaults: map[string]string{"acme/site": "main"}}
	})
	f.claimInstallation(5, f.web.ID, "acme/site")
	f.override(`{"deployments_per_day": 2}`)
	code, body := f.call("bob", "POST", "/api/apps/import", map[string]string{"repo": "acme/site", "team": "web"})
	if code != 201 || !strings.Contains(body, `"warning":"app created, but not deployed: plan limit reached`) ||
		strings.Contains(body, `"deployment":{`) {
		t.Fatalf("import: %d %s", code, body)
	}
}

// Suspension: the team cannot deploy or create, its apps are scaled to
// zero, and a suspended user's tokens stop working.
func TestSuspensionAPI(t *testing.T) {
	f := setupTeams(t)
	ref, sus := f.planServer(nil)
	ctx := context.Background()
	blog, _ := f.st.GetAppByName(ctx, "blog")
	queued, _, _ := f.st.EnqueueDeployment(ctx, blog.ID, sha(30), "main", "queued")

	code, body := f.call("erin", "POST", "/api/admin/teams/web/suspend", map[string]string{"reason": "spam"})
	if code != 200 || !strings.Contains(body, `"suspended_reason":"spam"`) || !strings.Contains(body, `"stopped":1`) {
		t.Fatalf("suspend: %d %s", code, body)
	}
	if len(sus.suspended) != 1 || sus.suspended[0] != "blog" {
		t.Fatalf("scaled down: %v", sus.suspended)
	}
	if d, _ := f.st.GetDeployment(ctx, queued.ID); d.Status != store.StatusCanceled {
		t.Fatalf("queued deployment: %s", d.Status)
	}
	code, body = f.call("bob", "POST", "/api/apps", map[string]string{"name": "shop", "repo": "nisagwn/shop", "team": "web"})
	wantLimit(t, "create while suspended", code, body, 403, plans.LimitSuspended)
	code, body = f.pushTo("nisagwn/blog", "main", sha(31))
	wantLimit(t, "push while suspended", code, body, 403, plans.LimitSuspended)
	if got := ref.wait(t, 1); !strings.Contains(got[0], "suspended") {
		t.Fatalf("GitHub status: %v", got)
	}
	code, body = f.call("bob", "POST", fmt.Sprintf("/api/apps/blog/deployments/%d/redeploy", f.d1.ID), nil)
	wantLimit(t, "redeploy while suspended", code, body, 403, plans.LimitSuspended)
	// Reading still works; the team shows up as suspended in the usage.
	if code, b := f.call("bob", "GET", "/api/teams/web/usage", nil); code != 200 || !strings.Contains(b, `"suspended_at"`) {
		t.Fatalf("usage while suspended: %d %s", code, b)
	}

	if code, b := f.call("admin", "POST", "/api/admin/teams/web/unsuspend", nil); code != 200 || strings.Contains(b, `"suspended_at"`) {
		t.Fatalf("unsuspend: %d %s", code, b)
	}
	if len(sus.resumed) != 1 || sus.resumed[0] != "blog" {
		t.Fatalf("scaled back: %v", sus.resumed)
	}
	if code, b := f.call("bob", "POST", "/api/apps", map[string]string{"name": "shop", "repo": "nisagwn/shop", "team": "web"}); code != 201 {
		t.Fatalf("create after unsuspend: %d %s", code, b)
	}

	// Users.
	if code, b := f.call("erin", "POST", "/api/admin/users/bob/suspend", map[string]string{"reason": "bots"}); code != 200 ||
		!strings.Contains(b, `"suspended":true`) {
		t.Fatalf("suspend user: %d %s", code, b)
	}
	if code, b := f.call("bob", "GET", "/api/apps", nil); code != 403 || !strings.Contains(b, "suspended") {
		t.Fatalf("suspended user's token: %d %s", code, b)
	}
	if _, b := f.call("erin", "GET", "/api/admin/users/suspended", nil); !strings.Contains(b, `"login":"bob"`) {
		t.Fatalf("suspended users: %s", b)
	}
	if code, _ := f.call("erin", "POST", "/api/admin/users/erin/suspend", nil); code != 409 {
		t.Fatalf("suspending a platform admin: %d", code)
	}
	if code, _ := f.call("erin", "POST", "/api/admin/users/ghost/suspend", nil); code != 404 {
		t.Fatalf("unknown user: %d", code)
	}
	if code, _ := f.call("erin", "POST", "/api/admin/users/bob/unsuspend", nil); code != 200 {
		t.Fatalf("unsuspend user: %d", code)
	}
	if code, _ := f.call("bob", "GET", "/api/apps", nil); code != 200 {
		t.Fatalf("after unsuspend: %d", code)
	}
	if code, _ := f.call("erin", "POST", "/api/admin/teams/nope/suspend", nil); code != 404 {
		t.Fatalf("unknown team: %d", code)
	}
}

// Admins change plans and per-team limits; others cannot.
func TestAdminPlansAPI(t *testing.T) {
	f := setupTeams(t)
	f.planServer(nil)
	if code, _ := f.call("alice", "PUT", "/api/admin/teams/web/plan", map[string]string{"plan": "pro"}); code != 403 {
		t.Fatalf("team owner changed the plan: %d", code)
	}
	code, body := f.call("erin", "PUT", "/api/admin/teams/web/plan", map[string]any{"plan": "pro", "limits": map[string]any{"projects": 50, "memory": "4Gi"}})
	var st plans.TeamStatus
	json.Unmarshal([]byte(body), &st)
	if code != 200 || st.Plan != store.PlanPro || st.Limits.Projects != 50 || st.Limits.Memory != "4Gi" ||
		st.Limits.Domains != plans.DefaultCatalog()[store.PlanPro].Domains {
		t.Fatalf("set plan: %d %s", code, body)
	}
	// Without "limits" the overrides stay; null clears them.
	f.call("erin", "PUT", "/api/admin/teams/web/plan", map[string]any{"plan": "hobby"})
	if tp, _ := f.st.GetTeamPlan(context.Background(), f.web.ID); tp.Plan != store.PlanHobby || !strings.Contains(string(tp.Overrides), `"projects": 50`) {
		t.Fatalf("kept overrides: %+v %s", tp, tp.Overrides)
	}
	f.call("erin", "PUT", "/api/admin/teams/web/plan", map[string]any{"plan": "hobby", "limits": nil})
	if tp, _ := f.st.GetTeamPlan(context.Background(), f.web.ID); string(tp.Overrides) != "{}" {
		t.Fatalf("cleared overrides: %s", tp.Overrides)
	}
	for _, bad := range []map[string]any{
		{"plan": "gold"},
		{"plan": "pro", "limits": map[string]any{"gpus": 1}},
		{"plan": "pro", "limits": map[string]any{"memory": "lots"}},
		{"plan": "pro", "limits": []int{1}},
	} {
		if code, b := f.call("erin", "PUT", "/api/admin/teams/web/plan", bad); code != 400 {
			t.Errorf("%v: %d %s", bad, code, b)
		}
	}
	if code, _ := f.call("erin", "PUT", "/api/admin/teams/nope/plan", map[string]string{"plan": "pro"}); code != 404 {
		t.Fatalf("unknown team: %d", code)
	}

	// The team list carries plans, usage and owners.
	code, body = f.call("admin", "GET", "/api/admin/teams", nil)
	var list []api.AdminTeamView
	json.Unmarshal([]byte(body), &list)
	if code != 200 || len(list) != 3 {
		t.Fatalf("admin teams: %d %s", code, body)
	}
	for _, row := range list {
		if row.Slug == "web" && (row.Members != 3 || len(row.Owners) != 1 || row.Owners[0] != "alice" || row.Usage.Projects != 1) {
			t.Fatalf("web row: %+v", row)
		}
	}
	if code, _ := f.call("erin", "GET", "/api/admin/teams/web", nil); code != 200 {
		t.Fatalf("admin team: %d", code)
	}
}

// API writes and new projects are rate-limited; reads are not.
func TestRateLimitsAPI(t *testing.T) {
	f := setupTeams(t)
	f.planServer(func(s *api.Server) {
		s.Plans = nil
		s.Limits = &ratelimit.Set{
			Writes: ratelimit.New(ratelimit.Rule{Rate: 3, Per: time.Hour}, nil),
			Import: ratelimit.New(ratelimit.Rule{Rate: 1, Per: time.Hour}, nil),
		}
	})
	if code, b := f.call("bob", "POST", "/api/apps", map[string]string{"name": "one", "repo": "nisagwn/one", "team": "web"}); code != 201 {
		t.Fatalf("first project: %d %s", code, b)
	}
	if code, b := f.call("bob", "POST", "/api/apps", map[string]string{"name": "two", "repo": "nisagwn/two", "team": "web"}); code != 429 ||
		!strings.Contains(b, "too many new projects") {
		t.Fatalf("second project: %d %s", code, b)
	}
	// Two writes left in the bucket (three per hour, one used twice).
	for i := 0; i < 2; i++ {
		f.call("bob", "PUT", "/api/apps/blog/env", map[string]string{"K": "v"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "PUT", f.srv.URL+"/api/apps/blog/env", strings.NewReader(`{"K":"v"}`))
	req.Header.Set("Authorization", "Bearer "+f.tokens["bob"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("over the write limit: %d %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	for i := 0; i < 5; i++ {
		if code, _ := f.call("bob", "GET", "/api/apps/blog", nil); code != 200 {
			t.Fatalf("read limited: %d", code)
		}
	}
}
