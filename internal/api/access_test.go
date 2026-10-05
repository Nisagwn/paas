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
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

// teamFixture: team "web" (alice owner, bob member, carol viewer) owns app
// "blog"; dave owns team "ops" with app "infra". Each user has a token.
type teamFixture struct {
	t      *testing.T
	st     *store.Store
	srv    *httptest.Server
	tokens map[string]string // principal → bearer token ("" = anonymous)
	users  map[string]store.User
	web    store.Team
	d1, d2 store.Deployment
	ss     *auth.Sessions
	// Faz 19: live resource usage served by /usage.
	usage *fakeUsage
}

func setupTeams(t *testing.T) *teamFixture {
	t.Helper()
	st := testdb.Open(t)
	ctx := context.Background()
	f := &teamFixture{t: t, st: st, tokens: map[string]string{"anonymous": "", "admin": token}, users: map[string]store.User{}}
	for i, login := range []string{"alice", "bob", "carol", "dave", "erin"} {
		u, err := st.UpsertUser(ctx, int64(100+i), login, "", "")
		if err != nil {
			t.Fatal(err)
		}
		f.users[login] = u
		plain, hash, prefix := auth.NewAPIToken()
		if _, err := st.CreateAPIToken(ctx, u.ID, "test", hash, prefix, nil); err != nil {
			t.Fatal(err)
		}
		f.tokens[login] = plain
	}
	var err error
	if f.web, err = st.CreateTeam(ctx, "web", "Web", f.users["alice"].ID); err != nil {
		t.Fatal(err)
	}
	st.SetMember(ctx, f.web.ID, f.users["bob"].ID, store.RoleMember)
	st.SetMember(ctx, f.web.ID, f.users["carol"].ID, store.RoleViewer)
	ops, _ := st.CreateTeam(ctx, "ops", "Ops", f.users["dave"].ID)

	blog, err := st.CreateAppInTeam(ctx, f.web.ID, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	st.CreateAppInTeam(ctx, ops.ID, "infra", "nisagwn/infra", "main")
	prod := []store.AliasSpec{{Hostname: "blog." + domain, Kind: store.AliasProduction, Branch: "main"}}
	f.d1, _, _ = st.EnqueueDeployment(ctx, blog.ID, sha(1), "main", "one")
	f.d2, _, _ = st.EnqueueDeployment(ctx, blog.ID, sha(2), "main", "two")
	st.MarkReady(ctx, f.d1, prod)
	st.MarkReady(ctx, f.d2, prod)

	f.ss = auth.New(token)
	f.usage = &fakeUsage{}
	srv := httptest.NewServer((&api.Server{
		Store: st, Domain: domain, APIToken: token, WebhookSecret: secret,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Sessions: f.ss,
		RuntimeLogs: &fakeRuntime{lines: "hello"}, Usage: f.usage,
		Stream: api.StreamTiming{Poll: 50 * time.Millisecond, FinishGrace: 50 * time.Millisecond},
	}).Handler())
	t.Cleanup(srv.Close)
	f.srv = srv
	return f
}

// call sends a request as who and returns the status and body.
func (f *teamFixture) call(who, method, path string, body any) (int, string) {
	f.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, f.srv.URL+path, r)
	if tok := f.tokens[who]; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestAuthorizationMatrix checks every /api/* endpoint for each kind of
// caller. "other" is dave, owner of another team: he must get 404, never
// 403, so app names of other teams do not leak.
func TestAuthorizationMatrix(t *testing.T) {
	f := setupTeams(t)
	who := []string{"anonymous", "carol", "bob", "alice", "dave", "admin"} // viewer, member, owner, other team
	type want map[string]int
	read := want{"anonymous": 401, "carol": 200, "bob": 200, "alice": 200, "dave": 404, "admin": 200}
	write := want{"anonymous": 401, "carol": 403, "bob": 200, "alice": 200, "dave": 404, "admin": 200}
	owner := want{"anonymous": 401, "carol": 403, "bob": 403, "alice": 200, "dave": 404, "admin": 200}

	d := f.d1.ID
	cases := []struct {
		method, path string
		body         func(who string) any
		want         want
		// before resets state the request changes.
		before func()
	}{
		{"GET", "/api/apps/blog", nil, read, nil},
		{"GET", "/api/apps/blog/deployments", nil, read, nil},
		{"GET", "/api/apps/blog/env", nil, read, nil},
		{"GET", fmt.Sprintf("/api/apps/blog/deployments/%d/runtime-logs", d), nil, read, nil},
		{"GET", fmt.Sprintf("/api/deployments/%d", d), nil, read, nil},
		{"GET", fmt.Sprintf("/api/deployments/%d/logs", d), nil, read, nil},
		{"GET", fmt.Sprintf("/api/deployments/%d/logs/stream", d), nil, read, nil},
		{"PUT", "/api/apps/blog/env", func(string) any { return map[string]string{"KEY": "v"} }, write, nil},
		{"POST", "/api/apps/blog/rollback", func(string) any { return map[string]int64{"deployment_id": d} }, write, nil},
		// Faz 17: promote (d1 runs with production variables: alias move),
		// redeploy, cancel (d2 is ready: authorized callers get 409), hooks.
		{"POST", "/api/apps/blog/promote", func(string) any { return map[string]int64{"deployment_id": d} }, write, nil},
		{"POST", fmt.Sprintf("/api/apps/blog/deployments/%d/redeploy", d), nil,
			want{"anonymous": 401, "carol": 403, "bob": 202, "alice": 202, "dave": 404, "admin": 202}, nil},
		{"POST", fmt.Sprintf("/api/deployments/%d/cancel", f.d2.ID), nil,
			want{"anonymous": 401, "carol": 403, "bob": 409, "alice": 409, "dave": 404, "admin": 409}, nil},
		{"GET", "/api/apps/blog/hooks", nil, write, nil},
		{"POST", "/api/apps/blog/hooks", func(string) any { return map[string]string{"name": "cms"} },
			want{"anonymous": 401, "carol": 403, "bob": 201, "alice": 201, "dave": 404, "admin": 201}, nil},
		{"DELETE", "/api/apps/blog/hooks/999999", nil,
			want{"anonymous": 401, "carol": 403, "bob": 404, "alice": 404, "dave": 404, "admin": 404}, nil},
		// The trigger URL has no caller: its token is the credential (501:
		// no GitHub App in this fixture).
		{"POST", "/api/hooks/deploy/paas_hook_x", nil,
			want{"anonymous": 501, "carol": 501, "bob": 501, "alice": 501, "dave": 501, "admin": 501}, nil},
		// Faz 11 scale to zero setting.
		{"GET", "/api/apps/blog/scale-to-zero", nil, read, nil},
		{"PUT", "/api/apps/blog/scale-to-zero", func(string) any { return map[string]bool{"production": false} }, write, nil},
		// Faz 19 analytics, deployment health and resource usage: viewer.
		{"GET", "/api/apps/blog/analytics?range=1h", nil, read, nil},
		{"GET", "/api/apps/blog/health", nil, read, nil},
		{"GET", "/api/apps/blog/usage", nil, read, nil},
		// Faz 16 build settings: reading is viewer, changes are member.
		{"GET", "/api/apps/blog/settings", nil, read, nil},
		{"PUT", "/api/apps/blog/settings", func(string) any { return map[string]string{"framework": "nextjs"} }, write, nil},
		// Faz 12 custom domains: reading is viewer, changes are member.
		{"GET", "/api/apps/blog/domains", nil, read, nil},
		{"POST", "/api/apps/blog/domains", func(w string) any { return map[string]string{"hostname": "www-" + w + ".example.com"} },
			want{"anonymous": 401, "carol": 403, "bob": 201, "alice": 201, "dave": 404, "admin": 201}, nil},
		{"DELETE", "/api/apps/blog/domains/del.example.com", nil,
			want{"anonymous": 401, "carol": 403, "bob": 204, "alice": 204, "dave": 404, "admin": 204},
			func() {
				blog, _ := f.st.GetAppByName(context.Background(), "blog")
				f.st.AddDomain(context.Background(), blog.ID, "del.example.com", "token")
			}},
		{"POST", "/api/apps", func(w string) any {
			return map[string]string{"name": "app-" + w, "repo": "nisagwn/app-" + w, "team": "web"}
		}, want{"anonymous": 401, "carol": 403, "bob": 201, "alice": 201, "dave": 404, "admin": 201}, nil},
		// Faz 15: importing a repository of an installation team "web" claimed.
		{"POST", "/api/apps/import", func(w string) any { return map[string]string{"repo": "acme/imp-" + w, "team": "web"} },
			want{"anonymous": 401, "carol": 403, "bob": 201, "alice": 201, "dave": 404, "admin": 201},
			func() { f.claimInstallation(7, f.web.ID, "acme/imp-bob", "acme/imp-alice", "acme/imp-admin") }},
		{"GET", "/api/teams/web", nil, read, nil},
		{"PUT", "/api/teams/web/members/erin", func(string) any { return map[string]string{"role": "viewer"} }, owner, nil},
		{"DELETE", "/api/teams/web/members/erin", nil,
			want{"anonymous": 401, "carol": 403, "bob": 403, "alice": 204, "dave": 404, "admin": 204},
			func() { f.st.SetMember(context.Background(), f.web.ID, f.users["erin"].ID, store.RoleViewer) }},
		// Endpoints without a team in the path: any authenticated caller.
		{"GET", "/api/apps", nil, want{"anonymous": 401, "carol": 200, "bob": 200, "alice": 200, "dave": 200, "admin": 200}, nil},
		{"GET", "/api/me", nil, want{"anonymous": 401, "carol": 200, "bob": 200, "alice": 200, "dave": 200, "admin": 200}, nil},
		{"GET", "/api/teams", nil, want{"anonymous": 401, "carol": 200, "bob": 200, "alice": 200, "dave": 200, "admin": 200}, nil},
		{"GET", "/api/github/repos", nil, want{"anonymous": 401, "carol": 200, "bob": 200, "alice": 200, "dave": 200, "admin": 200}, nil},
		// Unknown apps look the same as other teams' apps.
		{"GET", "/api/apps/nope", nil, want{"anonymous": 401, "carol": 404, "dave": 404, "admin": 404}, nil},
	}
	for _, c := range cases {
		for _, w := range who {
			expect, ok := c.want[w]
			if !ok {
				continue
			}
			t.Run(fmt.Sprintf("%s %s as %s", c.method, c.path, w), func(t *testing.T) {
				if c.before != nil {
					c.before()
				}
				var body any
				if c.body != nil {
					body = c.body(w)
				}
				if got, b := f.call(w, c.method, c.path, body); got != expect {
					t.Fatalf("status %d, want %d: %s", got, expect, b)
				}
			})
		}
	}
}

func TestListingAndDefaultTeam(t *testing.T) {
	f := setupTeams(t)
	names := func(who string) string {
		_, body := f.call(who, "GET", "/api/apps", nil)
		var apps []store.App
		json.Unmarshal([]byte(body), &apps)
		var out []string
		for _, a := range apps {
			out = append(out, a.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names("carol"); got != "blog" {
		t.Fatalf("carol sees %q", got)
	}
	if got := names("dave"); got != "infra" {
		t.Fatalf("dave sees %q", got)
	}
	if got := names("admin"); got != "blog,infra" {
		t.Fatalf("admin sees %q", got)
	}

	// Without "team": the first team where the caller may create apps.
	code, body := f.call("bob", "POST", "/api/apps", map[string]string{"name": "shop", "repo": "nisagwn/shop"})
	if code != 201 || !strings.Contains(body, fmt.Sprintf(`"team_id":%d`, f.web.ID)) {
		t.Fatalf("default team: %d %s", code, body)
	}
	// A viewer-only user has no team to create apps in; erin has no team at all.
	if code, _ := f.call("carol", "POST", "/api/apps", map[string]string{"name": "x1", "repo": "o/x1"}); code != 403 {
		t.Fatalf("viewer without team: %d", code)
	}
	if code, _ := f.call("erin", "POST", "/api/apps", map[string]string{"name": "x2", "repo": "o/x2"}); code != 403 {
		t.Fatalf("no team: %d", code)
	}
}

func TestTeamManagementAPI(t *testing.T) {
	f := setupTeams(t)
	if code, b := f.call("erin", "POST", "/api/teams", map[string]string{"slug": "data", "name": "Data"}); code != 201 {
		t.Fatalf("create team: %d %s", code, b)
	}
	if code, _ := f.call("bob", "POST", "/api/teams", map[string]string{"slug": "data"}); code != 409 {
		t.Fatalf("duplicate team: %d", code)
	}
	if code, _ := f.call("bob", "POST", "/api/teams", map[string]string{"slug": "Bad Slug"}); code != 400 {
		t.Fatalf("bad slug: %d", code)
	}
	if code, _ := f.call("erin", "PUT", "/api/teams/data/members/bob", map[string]string{"role": "admin"}); code != 400 {
		t.Fatalf("bad role: %d", code)
	}
	// Unknown login, no GitHub directory configured.
	if code, _ := f.call("erin", "PUT", "/api/teams/data/members/ghost", map[string]string{"role": "member"}); code != 404 {
		t.Fatalf("unknown user: %d", code)
	}
	if code, _ := f.call("erin", "PUT", "/api/teams/data/members/BOB", map[string]string{"role": "owner"}); code != 200 {
		t.Fatalf("add bob: %d", code)
	}
	// Bob is now an owner; erin may leave, but the last owner may not.
	if code, _ := f.call("erin", "DELETE", "/api/teams/data/members/erin", nil); code != 204 {
		t.Fatalf("leave: %d", code)
	}
	if code, b := f.call("bob", "PUT", "/api/teams/data/members/bob", map[string]string{"role": "viewer"}); code != 409 {
		t.Fatalf("demote last owner: %d %s", code, b)
	}
	if code, _ := f.call("carol", "DELETE", "/api/teams/web/members/carol", nil); code != 204 {
		t.Fatalf("viewer leaves: %d", code)
	}
	if code, _ := f.call("carol", "GET", "/api/apps/blog", nil); code != 404 {
		t.Fatalf("after leaving: %d", code)
	}
}

func TestPersonalTokensAPI(t *testing.T) {
	f := setupTeams(t)
	code, body := f.call("bob", "POST", "/api/tokens", map[string]any{"name": "ci", "expires_in_days": 30})
	var created struct {
		ID        int64      `json:"id"`
		Token     string     `json:"token"`
		Prefix    string     `json:"prefix"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	json.Unmarshal([]byte(body), &created)
	if code != 201 || !strings.HasPrefix(created.Token, "paas_") || created.ExpiresAt == nil ||
		!strings.HasPrefix(created.Token, created.Prefix) {
		t.Fatalf("create: %d %s", code, body)
	}
	f.tokens["bob-ci"] = created.Token
	if code, _ := f.call("bob-ci", "GET", "/api/apps/blog", nil); code != 200 {
		t.Fatalf("new token: %d", code)
	}
	// The plain token is never listed again.
	if _, body := f.call("bob", "GET", "/api/tokens", nil); strings.Contains(body, created.Token) || !strings.Contains(body, created.Prefix) {
		t.Fatalf("listing: %s", body)
	}
	// Someone else cannot revoke it; bob can.
	if code, _ := f.call("alice", "DELETE", fmt.Sprintf("/api/tokens/%d", created.ID), nil); code != 404 {
		t.Fatalf("foreign revoke: %d", code)
	}
	if code, _ := f.call("bob", "DELETE", fmt.Sprintf("/api/tokens/%d", created.ID), nil); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ := f.call("bob-ci", "GET", "/api/apps/blog", nil); code != 401 {
		t.Fatalf("revoked token: %d", code)
	}

	// Expired tokens and malformed ones are rejected.
	past := time.Now().Add(-time.Hour)
	plain, hash, prefix := auth.NewAPIToken()
	f.st.CreateAPIToken(context.Background(), f.users["bob"].ID, "old", hash, prefix, &past)
	f.tokens["expired"], f.tokens["bogus"] = plain, "paas_bogus"
	for _, w := range []string{"expired", "bogus"} {
		if code, _ := f.call(w, "GET", "/api/apps", nil); code != 401 {
			t.Fatalf("%s token: %d", w, code)
		}
	}
	// The admin has no user, hence no personal tokens.
	if code, _ := f.call("admin", "POST", "/api/tokens", map[string]string{"name": "x"}); code != 400 {
		t.Fatalf("admin token: %d", code)
	}
	if code, _ := f.call("bob", "POST", "/api/tokens", map[string]any{"name": "x", "expires_in_days": -1}); code != 400 {
		t.Fatalf("negative expiry: %d", code)
	}
}

// A user's session cookie authorizes read-only requests (EventSource) with
// the user's roles; unsafe methods still need a bearer token.
func TestSessionCookieUsesUserRoles(t *testing.T) {
	f := setupTeams(t)
	cookie := func(uid int64) string {
		rec := httptest.NewRecorder()
		f.ss.IssueUser(rec, httptest.NewRequest("GET", "/", nil), uid)
		c := rec.Result().Cookies()[0]
		return c.Name + "=" + c.Value
	}
	get := func(c, method, path string) int {
		req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(`{"KEY":"v"}`))
		req.Header.Set("Cookie", c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(cookie(f.users["carol"].ID), "GET", "/api/apps/blog"); code != 200 {
		t.Fatalf("carol cookie: %d", code)
	}
	if code := get(cookie(f.users["dave"].ID), "GET", "/api/apps/blog"); code != 404 {
		t.Fatalf("dave cookie: %d", code)
	}
	if code := get(cookie(f.users["alice"].ID), "PUT", "/api/apps/blog/env"); code != 401 {
		t.Fatalf("cookie PUT: %d", code)
	}
	if code := get(cookie(999999), "GET", "/api/apps"); code != 401 {
		t.Fatalf("deleted user: %d", code)
	}
}
