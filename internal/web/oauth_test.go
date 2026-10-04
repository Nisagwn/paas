package web_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/web"
)

// fakeGitHub plays github.com (authorize, token exchange) and its API.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	// challenges by authorization code (the code is the login to sign in as)
	challenge string
	users     map[string]int64    // login → id
	orgs      map[string][]string // login → orgs
	exchanges int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, users: map[string]int64{"root": 1, "orgie": 2, "stranger": 3, "friend": 4},
		orgs: map[string][]string{"orgie": {"other", "ACME"}, "stranger": {"evil"}}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.exchanges++
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		switch {
		case r.PostFormValue("client_id") != "cid" || r.PostFormValue("client_secret") != "csecret":
			json.NewEncoder(w).Encode(map[string]string{"error": "incorrect_client_credentials"})
		case base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge:
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "PKCE"})
		case !strings.HasSuffix(r.PostFormValue("redirect_uri"), "/auth/github/callback"):
			json.NewEncoder(w).Encode(map[string]string{"error": "redirect_uri_mismatch"})
		default:
			json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-" + r.PostFormValue("code"), "token_type": "bearer"})
		}
	})
	user := func(w http.ResponseWriter, login string) {
		id, ok := f.users[login]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(github.User{ID: id, Login: login, Name: strings.ToUpper(login)})
	}
	authed := func(r *http.Request) string {
		return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
	}
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) { user(w, authed(r)) })
	mux.HandleFunc("GET /user/orgs", func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]string
		for _, o := range f.orgs[authed(r)] {
			out = append(out, map[string]string{"login": o})
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /users/{login}", func(w http.ResponseWriter, r *http.Request) { user(w, r.PathValue("login")) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

type oauthUI struct {
	*ui
	gh *fakeGitHub
}

func setupOAuth(t *testing.T) *oauthUI {
	st := testdb.Open(t)
	gh := newFakeGitHub(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sessions := auth.NewWithKey(strings.Repeat("s", 32), "") // no legacy token
	s := &web.Server{
		Store: st, Sessions: sessions, Domain: "paas.test", Log: log,
		Auth: &auth.Authenticator{Store: st, Sessions: sessions, OAuth: true, Log: log,
			Users: auth.GitHubDirectory{Client: github.New(gh.srv.URL, "", log)}},
	}
	srv := httptest.NewServer(nil)
	s.GitHub = &auth.GitHubLogin{
		App:    &github.OAuthApp{ClientID: "cid", ClientSecret: "csecret", WebURL: gh.srv.URL},
		APIURL: gh.srv.URL, RedirectURI: srv.URL + "/auth/github/callback",
		Admins: []string{"Root"}, AllowedOrgs: []string{"acme"}, Log: log,
	}
	srv.Config.Handler = s.Handler()
	t.Cleanup(srv.Close)
	return &oauthUI{ui: &ui{t: t, st: st, srv: srv, client: newClient()}, gh: gh}
}

func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// signIn runs the browser side of the flow as GitHub user login and returns
// the callback's status and body.
func (u *oauthUI) signIn(login string) (int, string) {
	u.t.Helper()
	code, _, h := u.get("/auth/github?next=/teams")
	if code != http.StatusFound {
		u.t.Fatalf("start: %d", code)
	}
	loc, _ := url.Parse(h.Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(h.Get("Location"), u.gh.srv.URL+"/login/oauth/authorize?") ||
		q.Get("client_id") != "cid" || q.Get("code_challenge_method") != "S256" || q.Get("scope") != "read:org" {
		u.t.Fatalf("authorize URL %s", loc)
	}
	u.gh.mu.Lock()
	u.gh.challenge = q.Get("code_challenge")
	u.gh.mu.Unlock()
	code, body, _ := u.get("/auth/github/callback?code=" + login + "&state=" + url.QueryEscape(q.Get("state")))
	return code, body
}

func TestGitHubLogin(t *testing.T) {
	u := setupOAuth(t)
	ctx := context.Background()

	// Dev-mode token login is gone once GitHub login is configured.
	if _, body, _ := u.get("/login"); !strings.Contains(body, "Sign in with GitHub") || strings.Contains(body, `name="token"`) {
		t.Fatalf("login page:\n%s", body)
	}
	if code, _, _ := u.post("/login", url.Values{"token": {""}}, "self"); code != http.StatusNotFound {
		t.Fatalf("token login: %d, want 404", code)
	}

	// Admin bootstrap: allowed and made owner of the "default" team.
	code, body := u.signIn("root")
	if code != 200 || !strings.Contains(body, `url=/teams`) {
		t.Fatalf("admin login: %d\n%s", code, body)
	}
	root, err := u.st.UserByLogin(ctx, "root")
	if err != nil || root.GitHubID != 1 || root.Name != "ROOT" {
		t.Fatalf("user row: %+v %v", root, err)
	}
	def, _ := u.st.GetTeamBySlug(ctx, store.DefaultTeam)
	if role, _ := u.st.TeamRole(ctx, root.ID, def.ID); role != store.RoleOwner {
		t.Fatalf("admin role in default team = %q", role)
	}
	if code, body, _ := u.get("/teams"); code != 200 || !strings.Contains(body, "@root") {
		t.Fatalf("signed in: %d", code)
	}

	// The owner invites "friend", who is on no allowlist; GitHub resolves the login.
	csrf := u.csrf("/teams/default")
	if code, body, _ := u.post("/teams/default/members", url.Values{"login": {"friend"}, "role": {"viewer"}, "csrf": {csrf}}, "self"); code != http.StatusSeeOther {
		t.Fatalf("invite: %d\n%s", code, body)
	}

	// Organization member: allowed (org names compare case-insensitively).
	other := &oauthUI{ui: &ui{t: t, st: u.st, srv: u.srv, client: newClient()}, gh: u.gh}
	if code, body := other.signIn("orgie"); code != 200 {
		t.Fatalf("org member: %d\n%s", code, body)
	}
	// Stranger: refused, no session, no user row.
	stranger := &oauthUI{ui: &ui{t: t, st: u.st, srv: u.srv, client: newClient()}, gh: u.gh}
	if code, body := stranger.signIn("stranger"); code != http.StatusForbidden || !strings.Contains(body, "not allowed") {
		t.Fatalf("stranger: %d", code)
	}
	if code, _, _ := stranger.get("/"); code != http.StatusSeeOther {
		t.Fatalf("stranger got a session: %d", code)
	}
	if _, err := u.st.UserByLogin(ctx, "stranger"); err == nil {
		t.Fatal("refused user was recorded")
	}
	// Invited user: allowed.
	friend := &oauthUI{ui: &ui{t: t, st: u.st, srv: u.srv, client: newClient()}, gh: u.gh}
	if code, _ := friend.signIn("friend"); code != 200 {
		t.Fatalf("invited user: %d", code)
	}
	if code, _, _ := friend.get("/teams/default"); code != 200 {
		t.Fatalf("invited user's team page: %d", code)
	}
}

func TestGitHubLoginRejectsBadState(t *testing.T) {
	u := setupOAuth(t)
	// No state cookie at all (login started elsewhere: login CSRF).
	if code, _, _ := u.get("/auth/github/callback?code=root&state=forged"); code != http.StatusBadRequest {
		t.Fatalf("no cookie: %d", code)
	}
	// Cookie present, state differs.
	u.get("/auth/github")
	if code, _, _ := u.get("/auth/github/callback?code=root&state=forged"); code != http.StatusBadRequest {
		t.Fatalf("wrong state: %d", code)
	}
	// User cancelled on GitHub.
	_, _, h := u.get("/auth/github")
	loc, _ := url.Parse(h.Get("Location"))
	if code, _, _ := u.get("/auth/github/callback?error=access_denied&state=" + url.QueryEscape(loc.Query().Get("state"))); code != http.StatusUnauthorized {
		t.Fatalf("denied: %d", code)
	}
	if u.gh.exchanges != 0 {
		t.Fatalf("code exchanged %d times for rejected callbacks", u.gh.exchanges)
	}
	// A wrong PKCE verifier fails the exchange (the fake checks it).
	u.gh.challenge = "nope"
	_, _, h = u.get("/auth/github")
	loc, _ = url.Parse(h.Get("Location"))
	if code, _, _ := u.get("/auth/github/callback?code=root&state=" + url.QueryEscape(loc.Query().Get("state"))); code != http.StatusBadGateway {
		t.Fatalf("bad verifier: %d", code)
	}
}
