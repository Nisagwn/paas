package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
)

// installFlow runs the browser side of "Install GitHub App" for team as the
// signed-in user, with GitHub authorizing code (the GitHub login), and
// returns the final callback's status and body.
func (u *oauthUI) installFlow(team string, installation int64, code string) (int, string) {
	u.t.Helper()
	status, body, h := u.get("/github/install?team=" + team)
	if status != http.StatusFound {
		return status, body
	}
	loc, _ := url.Parse(h.Get("Location"))
	if !strings.HasPrefix(loc.String(), u.gh.srv.URL+"/apps/paas-test/installations/new?") {
		u.t.Fatalf("install URL %s", loc)
	}
	return u.setup(installation, "install", loc.Query().Get("state"), code)
}

// setup plays GitHub's redirect to the Setup URL and, when it starts the
// authorization, GitHub's redirect to the callback.
func (u *oauthUI) setup(installation int64, action, state, code string) (int, string) {
	u.t.Helper()
	q := url.Values{"installation_id": {itoa(installation)}, "setup_action": {action}, "state": {state}}
	status, body, h := u.get("/github/setup?" + q.Encode())
	if status != http.StatusFound {
		return status, body
	}
	loc, _ := url.Parse(h.Get("Location"))
	if !strings.HasPrefix(loc.String(), u.gh.srv.URL+"/login/oauth/authorize?") || loc.Query().Get("code_challenge_method") != "S256" {
		u.t.Fatalf("authorize URL %s", loc)
	}
	u.gh.mu.Lock()
	u.gh.challenge = loc.Query().Get("code_challenge")
	u.gh.mu.Unlock()
	status, body, _ = u.get("/auth/github/callback?code=" + code + "&state=" + url.QueryEscape(loc.Query().Get("state")))
	return status, body
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestGitHubAppClaim(t *testing.T) {
	u := setupOAuth(t)
	ctx := context.Background()
	u.gh.installs["root"] = []int64{42, 77}
	u.gh.repos[42] = []map[string]any{{"id": 1, "full_name": "acme/Web", "private": true}, {"id": 2, "full_name": "acme/api"}}
	if code, _ := u.signIn("root"); code != 200 {
		t.Fatalf("sign in: %d", code)
	}
	def, _ := u.st.GetTeamBySlug(ctx, store.DefaultTeam)

	// The import page offers the install button to members.
	if code, body, _ := u.get("/import"); code != 200 || !strings.Contains(body, "Install GitHub App") {
		t.Fatalf("import page: %d\n%s", code, body)
	}

	// Success, before the installation webhook arrived: the row is created
	// from GitHub's answer, with the repositories the user can see.
	code, body := u.installFlow("default", 42, "root")
	if code != 200 || !strings.Contains(body, "url=/import?ok=github") {
		t.Fatalf("claim: %d\n%s", code, body)
	}
	in, err := u.st.GetInstallation(ctx, 42)
	if err != nil || in.TeamID == nil || *in.TeamID != def.ID || in.AccountLogin != "acme" || in.AccountType != "Organization" {
		t.Fatalf("installation: %+v %v", in, err)
	}
	if code, body, _ := u.get("/import?ok=github"); code != 200 || !strings.Contains(body, "acme/Web") ||
		!strings.Contains(body, "linked to your team") || !strings.Contains(body, `value="web"`) {
		t.Fatalf("import page after claim: %d\n%s", code, body)
	}

	// Import through the form: CSRF and Origin are checked like every form.
	form := url.Values{"repo": {"acme/web"}, "team": {"default"}, "name": {"web"}}
	if code, _, _ := u.post("/import", form, "self"); code != http.StatusForbidden {
		t.Fatalf("import without CSRF: %d", code)
	}
	form.Set("csrf", u.csrf("/import"))
	if code, _, _ := u.post("/import", form, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin import: %d", code)
	}
	code, body, h := u.post("/import", form, "self")
	if code != http.StatusSeeOther || h.Get("Location") != "/apps/web?ok=imported-idle" {
		t.Fatalf("import: %d %s\n%s", code, h.Get("Location"), body)
	}
	if app, err := u.st.GetAppByName(ctx, "web"); err != nil || app.Repo != "acme/Web" || app.TeamID != def.ID {
		t.Fatalf("imported app: %+v %v", app, err)
	}
	if _, body, _ := u.get("/import"); !strings.Contains(body, "already imported as") {
		t.Fatal("imported repository not marked")
	}
	if code, body, _ := u.post("/import", form, "self"); code != http.StatusConflict || !strings.Contains(body, "already deployed as app web") {
		t.Fatalf("import twice: %d", code)
	}

	// Webhook first: the claim keeps the existing row and its repositories.
	u.st.UpsertInstallation(ctx, store.Installation{ID: 77, AccountLogin: "acme", AccountType: "Organization"})
	u.st.SetInstallationRepos(ctx, 77, []store.InstallationRepo{{RepoID: 9, FullName: "acme/docs"}})
	if code, body := u.installFlow("default", 77, "root"); code != 200 {
		t.Fatalf("claim existing: %d\n%s", code, body)
	}
	if repos, _ := u.st.ImportableRepos(ctx, []int64{def.ID}); len(repos) != 3 {
		t.Fatalf("importable after second claim: %+v", repos)
	}
}

func TestGitHubAppClaimRefused(t *testing.T) {
	u := setupOAuth(t)
	ctx := context.Background()
	u.gh.installs["root"] = []int64{42}
	u.gh.installs["orgie"] = []int64{55}
	u.gh.repos[42] = []map[string]any{{"id": 1, "full_name": "acme/web"}}
	if code, _ := u.signIn("orgie"); code != 200 {
		t.Fatalf("sign in: %d", code)
	}
	orgie, _ := u.st.UserByLogin(ctx, "orgie")
	team, _ := u.st.CreateTeam(ctx, "web", "Web", orgie.ID)
	claimed := func(id int64) bool {
		in, err := u.st.GetInstallation(ctx, id)
		return err == nil && in.TeamID != nil
	}

	// A valid session and state, but orgie's GitHub account cannot see 42:
	// the installation id alone must not be enough.
	if code, body := u.installFlow("web", 42, "orgie"); code != http.StatusForbidden || !strings.Contains(body, "cannot access") {
		t.Fatalf("foreign installation: %d\n%s", code, body)
	}
	if _, err := u.st.GetInstallation(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused claim recorded the installation: %v", err)
	}
	// Orgie's state, but GitHub authorizes another account (root) that can.
	if code, body := u.installFlow("web", 42, "root"); code != http.StatusForbidden || !strings.Contains(body, "different account") {
		t.Fatalf("wrong account: %d\n%s", code, body)
	}

	// Forged, expired and wrong-purpose states never reach GitHub.
	other := auth.NewWithKey(strings.Repeat("s", 32), "")
	for name, state := range map[string]string{
		"forged":        "forged",
		"expired":       other.Seal("github-install", itoa(team.ID)+"."+itoa(orgie.ID), -time.Minute),
		"wrong purpose": other.Seal("oauth", itoa(team.ID)+"."+itoa(orgie.ID), time.Minute),
		"other key":     auth.NewWithKey(strings.Repeat("x", 32), "").Seal("github-install", itoa(team.ID)+"."+itoa(orgie.ID), time.Minute),
	} {
		if code, _ := u.setup(55, "install", state, "orgie"); code != http.StatusBadRequest {
			t.Errorf("%s state: %d, want 400", name, code)
		}
	}
	// "Configure" on github.com without a state: back to the import page.
	if code, body := u.setup(55, "update", "", "orgie"); code != 200 || !strings.Contains(body, "url=/import?ok=github-updated") {
		t.Fatalf("update without state: %d\n%s", code, body)
	}
	if claimed(55) {
		t.Fatal("bad states claimed 55")
	}

	// A member who was demoted to viewer after getting the link is refused;
	// viewers get no link at all.
	_, _, h := u.get("/github/install?team=web")
	state := mustQuery(t, h.Get("Location"), "state")
	friend, _ := u.st.UpsertUser(ctx, 4, "friend", "", "")
	u.st.SetMember(ctx, team.ID, friend.ID, store.RoleOwner)
	u.st.SetMember(ctx, team.ID, orgie.ID, store.RoleViewer)
	if code, body := u.setup(55, "install", state, "orgie"); code != http.StatusForbidden || !strings.Contains(body, "member role") {
		t.Fatalf("viewer claim: %d\n%s", code, body)
	}
	if code, _, _ := u.get("/github/install?team=web"); code != http.StatusForbidden {
		t.Fatalf("viewer install link: %d", code)
	}
	if code, body, _ := u.get("/import"); code != 200 || strings.Contains(body, "Install GitHub App") {
		t.Fatalf("viewer import page: %d", code)
	}
	if claimed(55) {
		t.Fatal("viewer claimed 55")
	}

	// Cancelled on GitHub.
	u.st.SetMember(ctx, team.ID, orgie.ID, store.RoleMember)
	_, _, h = u.get("/github/install?team=web")
	_, _, h = u.get("/github/setup?installation_id=55&setup_action=install&state=" + url.QueryEscape(mustQuery(t, h.Get("Location"), "state")))
	oauthState := mustQuery(t, h.Get("Location"), "state")
	if code, _, _ := u.get("/auth/github/callback?error=access_denied&state=" + url.QueryEscape(oauthState)); code != http.StatusUnauthorized {
		t.Fatalf("cancelled: %d", code)
	}
	// And finally the real thing works for orgie's own installation.
	if code, body := u.installFlow("web", 55, "orgie"); code != 200 {
		t.Fatalf("own installation: %d\n%s", code, body)
	}
	if !claimed(55) {
		t.Fatal("55 not claimed")
	}
}

func mustQuery(t *testing.T, raw, key string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Query().Get(key) == "" {
		t.Fatalf("no %s in %q", key, raw)
	}
	return u.Query().Get(key)
}

// Without GitHub sign-in (development mode) the setup URL and the import
// page explain the manual path.
func TestImportPageDevMode(t *testing.T) {
	u := setup(t)
	u.login()
	if code, body, _ := u.get("/github/setup?installation_id=1&setup_action=install&state=x"); code != http.StatusNotImplemented ||
		!strings.Contains(body, "GitHub sign-in") {
		t.Fatalf("setup in dev mode: %d\n%s", code, body)
	}
	code, body, _ := u.get("/import")
	if code != 200 || !strings.Contains(body, "No GitHub App is configured") || strings.Contains(body, "Install GitHub App") {
		t.Fatalf("import page: %d\n%s", code, body)
	}
	if _, body, _ := u.get("/"); !strings.Contains(body, `href="/import"`) {
		t.Fatal("apps page has no Import button")
	}
	if code, _, _ := u.get("/github/install?team=default"); code != http.StatusNotFound {
		t.Fatalf("install in dev mode: %d", code)
	}
}
