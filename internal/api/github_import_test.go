package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/store"
)

// claimInstallation records installation id with repos, claimed by team.
func (f *teamFixture) claimInstallation(id, team int64, repos ...string) {
	f.t.Helper()
	ctx := context.Background()
	owner, _, _ := strings.Cut(repos[0], "/")
	if err := f.st.UpsertInstallation(ctx, store.Installation{ID: id, AccountLogin: owner, AccountType: "Organization"}); err != nil {
		f.t.Fatal(err)
	}
	var rs []store.InstallationRepo
	for i, r := range repos {
		rs = append(rs, store.InstallationRepo{RepoID: id*1000 + int64(i), FullName: r})
	}
	if err := f.st.AddInstallationRepos(ctx, id, rs); err != nil {
		f.t.Fatal(err)
	}
	if team != 0 {
		if err := f.st.ClaimInstallation(ctx, id, team); err != nil {
			f.t.Fatal(err)
		}
	}
}

// fakeRepos is a RepoInspector: heads by "repo@branch".
type fakeRepos struct {
	mu       sync.Mutex
	heads    map[string]string
	defaults map[string]string
}

func (f *fakeRepos) BranchHead(_ context.Context, repo, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sha, ok := f.heads[strings.ToLower(repo)+"@"+branch]; ok {
		return sha, nil
	}
	return "", errors.New("github: 404 Branch not found")
}

func (f *fakeRepos) DefaultBranch(_ context.Context, repo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.defaults[strings.ToLower(repo)]; ok {
		return b, nil
	}
	return "", errors.New("github: 404")
}

func TestImportApp(t *testing.T) {
	f := setupTeams(t)
	ctx := context.Background()
	ops, _ := f.st.GetTeamBySlug(ctx, "ops")
	f.claimInstallation(42, f.web.ID, "acme/Web-Site", "acme/1password", "acme/blog-two", "acme/broken")
	f.claimInstallation(43, 0, "other/secret") // not claimed by any team
	f.claimInstallation(44, ops.ID, "ops/tool")

	gh := &fakeRepos{
		heads:    map[string]string{"acme/web-site@trunk": sha(10), "acme/1password@main": sha(11)},
		defaults: map[string]string{"acme/web-site": "trunk", "acme/broken": "main"},
	}
	srv := httptest.NewServer((&api.Server{
		Store: f.st, Domain: domain, APIToken: token, WebhookSecret: secret, Sessions: f.ss,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), GitHubApp: gh,
	}).Handler())
	t.Cleanup(srv.Close)
	withoutApp := f.srv
	f.srv = srv

	type result struct {
		App struct {
			Name             string `json:"name"`
			Repo             string `json:"repo"`
			ProductionBranch string `json:"production_branch"`
			TeamID           int64  `json:"team_id"`
		} `json:"app"`
		Deployment *struct {
			ID        int64  `json:"id"`
			CommitSHA string `json:"commit_sha"`
			Branch    string `json:"branch"`
			Status    string `json:"status"`
		} `json:"deployment"`
		Warning string `json:"warning"`
	}
	imp := func(who string, body map[string]string) (int, result, string) {
		t.Helper()
		code, raw := f.call(who, "POST", "/api/apps/import", body)
		var r result
		json.Unmarshal([]byte(raw), &r)
		return code, r, raw
	}

	// Name and branch derived; GitHub's spelling of the repo is kept; the
	// first deployment is the head of the default branch.
	code, r, raw := imp("bob", map[string]string{"repo": "ACME/web-site"})
	if code != 201 || r.App.Name != "web-site" || r.App.Repo != "acme/Web-Site" || r.App.ProductionBranch != "trunk" ||
		r.App.TeamID != f.web.ID || r.Deployment == nil || r.Deployment.CommitSHA != sha(10) ||
		r.Deployment.Branch != "trunk" || r.Deployment.Status != store.StatusQueued {
		t.Fatalf("import: %d %s", code, raw)
	}
	if d, err := f.st.GetDeployment(ctx, r.Deployment.ID); err != nil || d.CommitMessage != "Imported from GitHub" {
		t.Fatalf("queued deployment: %+v %v", d, err)
	}

	// The listing marks it imported; other teams' and unclaimed
	// installations are not listed.
	_, list := f.call("bob", "GET", "/api/github/repos", nil)
	var repos []api.ImportableView
	json.Unmarshal([]byte(list), &repos)
	if len(repos) != 4 || repos[3].FullName != "acme/Web-Site" || repos[3].App != "web-site" || repos[3].Team != "web" ||
		!repos[3].CanImport || strings.Contains(list, "other/secret") || strings.Contains(list, "ops/tool") {
		t.Fatalf("listing: %s", list)
	}
	if _, list := f.call("carol", "GET", "/api/github/repos", nil); !strings.Contains(list, `"can_import":false`) {
		t.Fatalf("viewer listing: %s", list)
	}
	if _, list := f.call("dave", "GET", "/api/github/repos", nil); !strings.Contains(list, "ops/tool") || strings.Contains(list, "acme/") {
		t.Fatalf("other team listing: %s", list)
	}

	// Explicit branch and derived name of a repo starting with a digit.
	code, r, raw = imp("alice", map[string]string{"repo": "acme/1password", "production_branch": "main", "team": "web"})
	if code != 201 || r.App.Name != "app-1password" || r.Deployment == nil || r.Deployment.CommitSHA != sha(11) {
		t.Fatalf("import 1password: %d %s", code, raw)
	}

	for _, c := range []struct {
		who  string
		body map[string]string
		want int
	}{
		{"bob", map[string]string{"repo": "acme/Web-Site", "name": "again"}, 409},            // repo already imported
		{"bob", map[string]string{"repo": "acme/blog-two", "name": "blog"}, 409},             // name taken
		{"bob", map[string]string{"repo": "acme/blog-two", "name": "Bad Name"}, 400},         // invalid name
		{"bob", map[string]string{"repo": "not a repo"}, 400},                                // invalid repo
		{"bob", map[string]string{"repo": "acme/blog-two", "production_branch": "a b"}, 400}, // invalid branch
		{"bob", map[string]string{"repo": "other/secret"}, 404},                              // unclaimed installation
		{"bob", map[string]string{"repo": "ops/tool"}, 404},                                  // another team's installation
		{"dave", map[string]string{"repo": "ops/tool", "team": "web"}, 404},                  // team not visible
		{"dave", map[string]string{"repo": "acme/blog-two"}, 404},                            // not importable for ops
		{"dave", map[string]string{"repo": "acme/blog-two", "team": "ops"}, 404},             // not importable for ops
		{"carol", map[string]string{"repo": "acme/blog-two"}, 403},                           // viewer
		{"erin", map[string]string{"repo": "acme/blog-two"}, 404},                            // no team
	} {
		if code, _, raw := imp(c.who, c.body); code != c.want {
			t.Errorf("%s %v: %d, want %d: %s", c.who, c.body, code, c.want, raw)
		}
	}
	if _, err := f.st.GetAppByName(ctx, "again"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("refused import created an app")
	}

	// The head cannot be read: the app exists, a push deploys it later.
	code, r, raw = imp("bob", map[string]string{"repo": "acme/broken"})
	if code != 201 || r.App.Name != "broken" || r.Deployment != nil || !strings.Contains(r.Warning, "push") {
		t.Fatalf("import without head: %d %s", code, raw)
	}

	// No GitHub App configured: the app is created without a deployment.
	f.srv = withoutApp
	code, r, raw = imp("bob", map[string]string{"repo": "acme/blog-two"})
	if code != 201 || r.App.Name != "blog-two" || r.App.ProductionBranch != "main" || r.Deployment != nil || r.Warning != "" {
		t.Fatalf("import without app: %d %s", code, raw)
	}
	app, _ := f.st.GetAppByName(ctx, "blog-two")
	if deps, _ := f.st.ListDeployments(ctx, app.ID, 10); len(deps) != 0 {
		t.Fatalf("deployments without app: %+v", deps)
	}
}

func TestDeriveAppName(t *testing.T) {
	for repo, want := range map[string]string{
		"o/blog":                                "blog",
		"o/My_Site.v2":                          "my-site-v2",
		"o/1password":                           "app-1password",
		"o/a":                                   "app-a",
		"o/--x--":                               "app-x",
		"o/___":                                 "app",
		"o/" + strings.Repeat("abc", 20):        "abcabcabcabcabcabcabcabcabcabca",
		"o/this-is-a-very-long-repository-name": "this-is-a-very-long-repository",
	} {
		if got := api.DeriveAppName(repo); got != want {
			t.Errorf("DeriveAppName(%q) = %q, want %q", repo, got, want)
		}
	}
}
