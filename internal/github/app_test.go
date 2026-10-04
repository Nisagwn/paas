package github_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

const appID = 4242

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

var foreignKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// otherKey is a key GitHub does not know: its JWTs are rejected.
func otherKey(*testing.T) *rsa.PrivateKey { return foreignKey() }

func pkcs1PEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey())}))
}

func TestParsePrivateKey(t *testing.T) {
	der, err := x509.MarshalPKCS8PrivateKey(testKey())
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for name, in := range map[string]string{
		"pkcs1":             pkcs1PEM(),
		"pkcs8":             pkcs8,
		"escaped newlines":  strings.ReplaceAll(strings.TrimSpace(pkcs1PEM()), "\n", `\n`),
		"quoted, escaped":   `"` + strings.ReplaceAll(pkcs8, "\n", `\n`) + `"`,
		"surrounding space": "\n  " + pkcs1PEM() + "\n\n",
	} {
		k, err := github.ParsePrivateKey([]byte(in))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !k.Equal(testKey()) {
			t.Errorf("%s: parsed a different key", name)
		}
	}

	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ec)
	for name, in := range map[string]string{
		"not pem":   "hello",
		"empty":     "",
		"ec key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})),
		"wrong pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}})),
		"bad der":   string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte{1, 2}})),
	} {
		if _, err := github.ParsePrivateKey([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type jwtClaims struct {
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Iss string `json:"iss"`
}

// verifyJWT checks an RS256 JWT against the test key, as GitHub would.
func verifyJWT(token string) (jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, fmt.Errorf("jwt has %d parts", len(parts))
	}
	enc := base64.RawURLEncoding
	var header struct{ Alg, Typ string }
	if b, err := enc.DecodeString(parts[0]); err != nil || json.Unmarshal(b, &header) != nil {
		return jwtClaims{}, fmt.Errorf("bad header %q", parts[0])
	}
	if header.Alg != "RS256" || header.Typ != "JWT" {
		return jwtClaims{}, fmt.Errorf("header = %+v", header)
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return jwtClaims{}, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey().PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		return jwtClaims{}, fmt.Errorf("signature: %w", err)
	}
	var c jwtClaims
	b, err := enc.DecodeString(parts[1])
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func TestJWT(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	app := github.NewApp("", appID, testKey(), nil)
	github.SetClock(app, func() time.Time { return now })
	tok, err := app.JWT()
	if err != nil {
		t.Fatal(err)
	}
	c, err := verifyJWT(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Iss != strconv.Itoa(appID) || c.Iat != now.Unix()-60 || c.Exp != now.Add(9*time.Minute).Unix() {
		t.Fatalf("claims = %+v", c)
	}
	// Signed with another key: rejected.
	forged, _ := github.NewApp("", appID, otherKey(t), nil).JWT()
	if _, err := verifyJWT(forged); err == nil {
		t.Fatal("JWT signed by another key verified")
	}
}

// fakeApp is GitHub's side of a GitHub App: it checks JWTs and installation
// tokens and serves installations, repositories and refs.
type fakeApp struct {
	t   *testing.T
	srv *httptest.Server
	app *App

	mu    sync.Mutex
	clock time.Time
	mints map[int64]int
	// gate, when set, holds token requests until closed.
	gate        chan struct{}
	repoInstall map[string]int64
	installs    []map[string]any
	repos       map[int64][]github.Repository
	statusAuth  []string
	lookups     int
	// rejectsExpected: a bad JWT is part of the test, not a failure.
	rejectsExpected bool
}

// App aliases github.App so fakeApp reads naturally.
type App = github.App

func newFakeApp(t *testing.T, lookup github.InstallationLookup) *fakeApp {
	f := &fakeApp{t: t, clock: time.Unix(1_800_000_000, 0), mints: map[int64]int{},
		repoInstall: map[string]int64{}, repos: map[int64][]github.Repository{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !f.appAuth(w, r) {
			return
		}
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		f.mu.Lock()
		gate := f.gate
		f.mints[id]++
		n, expires := f.mints[id], f.clock.Add(time.Hour)
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("inst-%d-%d", id, n), "expires_at": expires})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !f.appAuth(w, r) {
			return
		}
		f.mu.Lock()
		f.lookups++
		id, ok := f.repoInstall[r.PathValue("owner")+"/"+r.PathValue("repo")]
		f.mu.Unlock()
		if !ok {
			notFound(w)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id})
	})
	mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		if !f.appAuth(w, r) {
			return
		}
		f.mu.Lock()
		all := f.installs
		f.mu.Unlock()
		f.page(w, r, "/app/installations", len(all), func(i int) any { return []any{all[i]} })
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		id, ok := f.installationAuth(w, r)
		if !ok {
			return
		}
		f.mu.Lock()
		all := f.repos[id]
		f.mu.Unlock()
		f.page(w, r, "/installation/repositories", len(all), func(i int) any {
			return map[string]any{"total_count": len(all), "repositories": []github.Repository{all[i]}}
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.installationAuth(w, r); !ok {
			return
		}
		heads := map[string]string{"main": sha, "feature/login": strings.Repeat("b", 40)}
		h, ok := heads[r.PathValue("branch")]
		if !ok {
			notFound(w)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/" + r.PathValue("branch"), "object": map[string]string{"sha": h}})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.installationAuth(w, r); !ok {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"full_name": r.PathValue("owner") + "/" + r.PathValue("repo"), "default_branch": "trunk"})
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/statuses/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.statusAuth = append(f.statusAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	f.app = github.NewApp(f.srv.URL, appID, testKey(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.app.Installations = lookup
	github.SetClock(f.app, f.now)
	return f
}

func (f *fakeApp) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *fakeApp) advance(d time.Duration) {
	f.mu.Lock()
	f.clock = f.clock.Add(d)
	f.mu.Unlock()
}

func (f *fakeApp) minted(id int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints[id]
}

// appAuth accepts a JWT signed by the App's key for its ID.
func (f *fakeApp) appAuth(w http.ResponseWriter, r *http.Request) bool {
	c, err := verifyJWT(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil || c.Iss != strconv.Itoa(appID) {
		if !f.rejectsExpected {
			f.t.Errorf("%s %s: bad app JWT: %v", r.Method, r.URL.Path, err)
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"A JSON web token could not be decoded"}`)
		return false
	}
	return true
}

// installationAuth accepts a token minted by access_tokens; it returns the
// installation it belongs to.
func (f *fakeApp) installationAuth(w http.ResponseWriter, r *http.Request) (int64, bool) {
	var id int64
	var n int
	if _, err := fmt.Sscanf(r.Header.Get("Authorization"), "Bearer inst-%d-%d", &id, &n); err != nil {
		f.t.Errorf("%s %s: Authorization = %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
		return 0, false
	}
	return id, true
}

// page serves one item per page, linking to the next one.
func (f *fakeApp) page(w http.ResponseWriter, r *http.Request, path string, n int, item func(int) any) {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if p == 0 {
		p = 1
	}
	if p < n {
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=1&page=%d>; rel="next"`, f.srv.URL, path, p+1))
	}
	if p > n {
		fmt.Fprint(w, `[]`)
		return
	}
	json.NewEncoder(w).Encode(item(p - 1))
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"message":"Not Found"}`)
}

func TestInstallationTokenCachedUntilNearExpiry(t *testing.T) {
	f := newFakeApp(t, nil)
	ctx := context.Background()

	tok, err := f.app.InstallationToken(ctx, 7)
	if err != nil || tok != "inst-7-1" {
		t.Fatalf("first token = %q, %v", tok, err)
	}
	f.advance(54 * time.Minute) // 6 minutes left: still cached
	if tok, _ := f.app.InstallationToken(ctx, 7); tok != "inst-7-1" || f.minted(7) != 1 {
		t.Fatalf("cached token = %q after %d mints", tok, f.minted(7))
	}
	// Another installation has its own token.
	if tok, _ := f.app.InstallationToken(ctx, 8); tok != "inst-8-1" {
		t.Fatalf("installation 8 token = %q", tok)
	}
	f.advance(2 * time.Minute) // 4 minutes left: refreshed
	if tok, _ := f.app.InstallationToken(ctx, 7); tok != "inst-7-2" || f.minted(7) != 2 {
		t.Fatalf("refreshed token = %q after %d mints", tok, f.minted(7))
	}
}

func TestInstallationTokenSingleFlight(t *testing.T) {
	f := newFakeApp(t, nil)
	f.gate = make(chan struct{})
	const callers = 20
	var wg sync.WaitGroup
	tokens := make([]string, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := f.app.InstallationToken(context.Background(), 7)
			if err != nil {
				t.Error(err)
			}
			tokens[i] = tok
		}()
	}
	// Let every caller queue up behind the one request, then release it.
	deadline := time.Now().Add(5 * time.Second)
	for f.minted(7) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(f.gate)
	wg.Wait()
	if n := f.minted(7); n != 1 {
		t.Fatalf("%d token requests for %d concurrent callers, want 1", n, callers)
	}
	for i, tok := range tokens {
		if tok != "inst-7-1" {
			t.Fatalf("caller %d got %q", i, tok)
		}
	}

	// A caller that gives up does not fail the shared request.
	f.advance(time.Hour)
	f.mu.Lock()
	f.gate = make(chan struct{})
	f.mu.Unlock()
	short, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.app.InstallationToken(short, 7)
		done <- err
	}()
	for f.minted(7) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	other := make(chan string, 1)
	go func() {
		tok, _ := f.app.InstallationToken(context.Background(), 7)
		other <- tok
	}()
	cancel()
	close(f.gate)
	<-done
	if tok := <-other; tok != "inst-7-2" {
		t.Fatalf("waiting caller got %q after the first caller's ctx ended", tok)
	}
}

func TestInstallationTokenError(t *testing.T) {
	f := newFakeApp(t, nil)
	f.app.Key = otherKey(t) // GitHub rejects the JWT
	f.rejectsExpected = true
	_, err := f.app.InstallationToken(context.Background(), 7)
	var apiErr *github.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want a 401", err)
	}
}

// mapLookup is an in-memory InstallationLookup.
type mapLookup map[string]int64

func (m mapLookup) InstallationForRepo(_ context.Context, repo string) (int64, error) {
	if id, ok := m[strings.ToLower(repo)]; ok {
		return id, nil
	}
	return 0, store.ErrNotFound
}

func TestRepoToken(t *testing.T) {
	f := newFakeApp(t, mapLookup{"acme/web": 7})
	f.repoInstall["acme/api"] = 9
	ctx := context.Background()

	// Mirrored in the store: no lookup on GitHub.
	if tok, err := f.app.RepoToken(ctx, "Acme/Web"); err != nil || tok != "inst-7-1" {
		t.Fatalf("store lookup: %q, %v", tok, err)
	}
	if f.lookups != 0 {
		t.Fatalf("%d GitHub lookups for a mirrored repo", f.lookups)
	}
	// Not mirrored: GitHub knows the installation.
	if tok, err := f.app.RepoToken(ctx, "acme/api"); err != nil || tok != "inst-9-1" {
		t.Fatalf("API fallback: %q, %v", tok, err)
	}
	// Not installed anywhere.
	if _, err := f.app.RepoToken(ctx, "acme/other"); !errors.Is(err, github.ErrNotInstalled) {
		t.Fatalf("not installed: %v", err)
	}
	if _, err := f.app.RepoToken(ctx, "nope"); err == nil {
		t.Fatal("invalid repo accepted")
	}

	// AppTokens falls back to the personal token, or none.
	if tok, err := (github.AppTokens{App: f.app, Fallback: "pat"}).RepoToken(ctx, "acme/other"); err != nil || tok != "pat" {
		t.Fatalf("fallback: %q, %v", tok, err)
	}
	if tok, err := (github.AppTokens{App: f.app}).RepoToken(ctx, "acme/other"); err != nil || tok != "" {
		t.Fatalf("no fallback: %q, %v", tok, err)
	}
	if tok, err := (github.AppTokens{App: f.app, Fallback: "pat"}).RepoToken(ctx, "acme/web"); err != nil || tok != "inst-7-1" {
		t.Fatalf("installed: %q, %v", tok, err)
	}
}

func TestBranchHeadAndDefaultBranch(t *testing.T) {
	f := newFakeApp(t, mapLookup{"acme/web": 7})
	ctx := context.Background()
	if h, err := f.app.BranchHead(ctx, "acme/web", "main"); err != nil || h != sha {
		t.Fatalf("main: %q, %v", h, err)
	}
	if h, err := f.app.BranchHead(ctx, "acme/web", "feature/login"); err != nil || h != strings.Repeat("b", 40) {
		t.Fatalf("branch with a slash: %q, %v", h, err)
	}
	if _, err := f.app.BranchHead(ctx, "acme/web", "gone"); !github.IsNotFound(err) {
		t.Fatalf("missing branch: %v, want a 404", err)
	}
	if b, err := f.app.DefaultBranch(ctx, "acme/web"); err != nil || b != "trunk" {
		t.Fatalf("default branch: %q, %v", b, err)
	}
	if _, err := f.app.DefaultBranch(ctx, "acme/other"); !errors.Is(err, github.ErrNotInstalled) {
		t.Fatalf("not installed: %v", err)
	}
	if f.minted(7) != 1 {
		t.Fatalf("%d tokens minted for one installation", f.minted(7))
	}
}

func TestListInstallationsPaginated(t *testing.T) {
	f := newFakeApp(t, nil)
	f.installs = []map[string]any{
		{"id": 1, "account": map[string]string{"login": "acme", "type": "Organization"}},
		{"id": 2, "account": map[string]string{"login": "nisa", "type": "User"}, "suspended_at": "2026-10-01T00:00:00Z"},
	}
	f.repos[1] = []github.Repository{{ID: 10, FullName: "acme/web"}, {ID: 11, FullName: "acme/api", Private: true}}
	ctx := context.Background()

	ins, err := f.app.ListInstallations(ctx)
	if err != nil || len(ins) != 2 {
		t.Fatalf("installations: %+v, %v", ins, err)
	}
	if ins[1].Account.Login != "nisa" || ins[1].SuspendedAt == nil || ins[0].SuspendedAt != nil {
		t.Fatalf("installations = %+v", ins)
	}
	repos, err := f.app.ListInstallationRepos(ctx, 1)
	if err != nil || len(repos) != 2 || repos[1].FullName != "acme/api" || !repos[1].Private {
		t.Fatalf("repos: %+v, %v", repos, err)
	}
}

// With the App, the notifier's statuses carry the installation token; a
// repository it is not installed on is skipped without an error.
func TestNotifierWithAppTokens(t *testing.T) {
	f := newFakeApp(t, mapLookup{"nisagwn/blog": 7})
	c := github.New(f.srv.URL, "", nil)
	c.Tokens = github.AppTokens{App: f.app}
	n := &github.Notifier{Client: c, PublicURL: "https://paas.test"}
	ctx := context.Background()

	if err := n.DeploymentStarted(ctx, deployment()); err != nil {
		t.Fatal(err)
	}
	if err := n.DeploymentFinished(ctx, deployment(), worker.Result{Status: store.StatusReady, URL: "https://x"}); err != nil {
		t.Fatal(err)
	}
	if len(f.statusAuth) != 2 || f.statusAuth[0] != "Bearer inst-7-1" || f.statusAuth[1] != "Bearer inst-7-1" {
		t.Fatalf("status Authorization headers = %q", f.statusAuth)
	}

	other := deployment()
	other.Repo = "someone/else"
	if err := n.DeploymentStarted(ctx, other); err != nil {
		t.Fatalf("uninstalled repo: %v", err)
	}
	if err := c.CreateCommitStatus(ctx, "someone/else", sha, github.Status{State: github.StateSuccess}); !errors.Is(err, github.ErrNoCredentials) {
		t.Fatalf("client without credentials: %v", err)
	}
	if len(f.statusAuth) != 2 {
		t.Fatalf("status sent without credentials: %q", f.statusAuth)
	}
}
