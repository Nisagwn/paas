package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

// Faz 15: the platform's GitHub App. It signs JWTs with the App's private
// key, trades them for installation access tokens (one hour, cached) and
// uses those for clones, commit statuses, PR comments and repository reads.

// ErrNotInstalled means the App is not installed on a repository (or its
// installation is suspended).
var ErrNotInstalled = errors.New("github: app not installed on repository")

// ErrNoCredentials is returned by a Client with a TokenSource that has no
// token for the repository: a call that would fail unauthenticated anyway.
var ErrNoCredentials = errors.New("github: no credentials for repository")

const (
	// jwtBackdate covers clock drift between us and GitHub.
	jwtBackdate = 60 * time.Second
	// jwtLifetime stays under GitHub's 10-minute maximum.
	jwtLifetime = 9 * time.Minute
	// tokenRefreshMargin: a cached installation token is replaced this long
	// before it expires, so a clone never starts with a token about to lapse.
	tokenRefreshMargin = 5 * time.Minute
)

// TokenSource hands out the token for GitHub calls about one repository
// ("owner/name"). An empty token with a nil error means "no credentials":
// public repositories still clone anonymously.
type TokenSource interface {
	RepoToken(ctx context.Context, repo string) (string, error)
}

// InstallationLookup finds the installation granting access to a repository;
// *store.Store implements it. It returns store.ErrNotFound when none does.
type InstallationLookup interface {
	InstallationForRepo(ctx context.Context, repo string) (int64, error)
}

// App is a GitHub App's identity. Its methods are safe for concurrent use.
type App struct {
	ID  int64
	Key *rsa.PrivateKey
	// Installations is consulted before asking GitHub which installation
	// covers a repository (optional).
	Installations InstallationLookup
	// api sends the requests; its Token is never set, every call passes the
	// JWT or an installation token explicitly.
	api *Client
	now func() time.Time

	mu       sync.Mutex
	tokens   map[int64]installationToken
	inflight map[int64]*tokenCall
}

type installationToken struct {
	token   string
	expires time.Time
}

// tokenCall is one in-flight token request that concurrent callers share.
type tokenCall struct {
	done  chan struct{}
	token installationToken
	err   error
}

// NewApp returns an App talking to the API at baseURL (DefaultBaseURL when
// empty).
func NewApp(baseURL string, id int64, key *rsa.PrivateKey, log *slog.Logger) *App {
	return &App{
		ID: id, Key: key,
		api:      New(baseURL, "", log),
		now:      time.Now,
		tokens:   map[int64]installationToken{},
		inflight: map[int64]*tokenCall{},
	}
}

// ParsePrivateKey decodes the App's PEM private key: PKCS#1 ("RSA PRIVATE
// KEY", what GitHub downloads) or PKCS#8 ("PRIVATE KEY"). Literal "\n"
// sequences, as left by a key pasted into a single-line env var, are read
// as newlines.
func ParsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	s := strings.TrimSpace(string(data))
	s = strings.Trim(s, `"'`)
	if !strings.Contains(s, "\n") {
		s = strings.ReplaceAll(s, `\n`, "\n")
	}
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("github app: private key is not PEM encoded")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("github app: private key: %w", err)
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("github app: private key: %w", err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("github app: private key is %T, want RSA", k)
		}
		return rk, nil
	default:
		return nil, fmt.Errorf("github app: unexpected PEM block %q", block.Type)
	}
}

// JWT returns a token authenticating as the App itself (RS256, valid for
// nine minutes), used for /app/* calls and to mint installation tokens.
func (a *App) JWT() (string, error) {
	now := a.now()
	header := `{"alg":"RS256","typ":"JWT"}`
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": strconv.FormatInt(a.ID, 10),
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("github app: sign jwt: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// InstallationToken returns an access token for an installation, from the
// cache while it is valid for more than five minutes. Concurrent callers for
// the same installation share one request to GitHub.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	a.mu.Lock()
	if t, ok := a.tokens[installationID]; ok && a.now().Add(tokenRefreshMargin).Before(t.expires) {
		a.mu.Unlock()
		return t.token, nil
	}
	call, running := a.inflight[installationID]
	if !running {
		call = &tokenCall{done: make(chan struct{})}
		a.inflight[installationID] = call
	}
	a.mu.Unlock()

	if !running {
		// Detached from the first caller's ctx so that its cancellation does
		// not fail the others; the client's own timeout still bounds it.
		call.token, call.err = a.createInstallationToken(context.WithoutCancel(ctx), installationID)
		a.mu.Lock()
		delete(a.inflight, installationID)
		if call.err == nil {
			a.tokens[installationID] = call.token
		}
		a.mu.Unlock()
		close(call.done)
	}
	select {
	case <-call.done:
		return call.token.token, call.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (a *App) createInstallationToken(ctx context.Context, id int64) (installationToken, error) {
	jwt, err := a.JWT()
	if err != nil {
		return installationToken{}, err
	}
	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", id)
	if _, err := a.api.request(ctx, jwt, http.MethodPost, path, nil, &body); err != nil {
		return installationToken{}, err
	}
	if body.Token == "" {
		return installationToken{}, fmt.Errorf("github app: POST %s: empty token", path)
	}
	return installationToken{token: body.Token, expires: body.ExpiresAt}, nil
}

// forget drops a cached token, e.g. after GitHub rejected it.
func (a *App) forget(installationID int64) {
	a.mu.Lock()
	delete(a.tokens, installationID)
	a.mu.Unlock()
}

// RepoInstallation returns the installation that covers repo: the one the
// store recorded, else GitHub's answer. ErrNotInstalled when there is none.
func (a *App) RepoInstallation(ctx context.Context, repo string) (int64, error) {
	if err := checkRepo(repo); err != nil {
		return 0, err
	}
	if a.Installations != nil {
		id, err := a.Installations.InstallationForRepo(ctx, repo)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return 0, err
		}
	}
	// Not (yet) mirrored, e.g. a webhook delivery was missed. The next
	// resync records it; until then GitHub is asked each time.
	jwt, err := a.JWT()
	if err != nil {
		return 0, err
	}
	var body struct {
		ID int64 `json:"id"`
	}
	if _, err := a.api.request(ctx, jwt, http.MethodGet, "/repos/"+repo+"/installation", nil, &body); err != nil {
		if IsNotFound(err) {
			return 0, fmt.Errorf("%w: %s", ErrNotInstalled, repo)
		}
		return 0, err
	}
	return body.ID, nil
}

// RepoToken returns an installation token that grants access to repo.
// ErrNotInstalled when the App is not installed on it.
func (a *App) RepoToken(ctx context.Context, repo string) (string, error) {
	id, err := a.RepoInstallation(ctx, repo)
	if err != nil {
		return "", err
	}
	return a.InstallationToken(ctx, id)
}

// AppTokens is the TokenSource used when the App is configured: the
// installation token for repositories the App is installed on, Fallback
// (the old personal access token, possibly empty) for the others, so
// repositories connected before the App keep working.
type AppTokens struct {
	App      *App
	Fallback string
}

func (t AppTokens) RepoToken(ctx context.Context, repo string) (string, error) {
	tok, err := t.App.RepoToken(ctx, repo)
	if errors.Is(err, ErrNotInstalled) {
		return t.Fallback, nil
	}
	return tok, err
}

// StaticToken is a TokenSource that returns the same token for every
// repository (the personal access token).
type StaticToken string

func (s StaticToken) RepoToken(context.Context, string) (string, error) { return string(s), nil }

// AppInstallation is an installation as GET /app/installations reports it.
type AppInstallation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	SuspendedAt *time.Time `json:"suspended_at"`
}

// Repository is a repository as the installation endpoints report it.
type Repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

// ListInstallations returns every installation of the App.
func (a *App) ListInstallations(ctx context.Context) ([]AppInstallation, error) {
	jwt, err := a.JWT()
	if err != nil {
		return nil, err
	}
	var all []AppInstallation
	for path := "/app/installations?per_page=100"; path != ""; {
		var page []AppInstallation
		next, err := a.api.request(ctx, jwt, http.MethodGet, path, nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		path = next
	}
	return all, nil
}

// ListInstallationRepos returns the repositories an installation grants.
func (a *App) ListInstallationRepos(ctx context.Context, installationID int64) ([]Repository, error) {
	tok, err := a.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var all []Repository
	for path := "/installation/repositories?per_page=100"; path != ""; {
		var page struct {
			Repositories []Repository `json:"repositories"`
		}
		next, err := a.api.request(ctx, tok, http.MethodGet, path, nil, &page)
		if err != nil {
			if isUnauthorized(err) {
				a.forget(installationID)
			}
			return nil, err
		}
		all = append(all, page.Repositories...)
		path = next
	}
	return all, nil
}

// BranchHead returns the commit SHA a branch of repo points at.
func (a *App) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	if err := a.repoGet(ctx, repo, "/repos/"+repo+"/git/ref/heads/"+strings.Join(segs, "/"), &ref); err != nil {
		return "", err
	}
	if ref.Object.SHA == "" {
		return "", fmt.Errorf("github: branch %s of %s has no commit", branch, repo)
	}
	return ref.Object.SHA, nil
}

// DefaultBranch returns repo's default branch.
func (a *App) DefaultBranch(ctx context.Context, repo string) (string, error) {
	var r Repository
	if err := a.repoGet(ctx, repo, "/repos/"+repo, &r); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

func (a *App) repoGet(ctx context.Context, repo, path string, out any) error {
	id, err := a.RepoInstallation(ctx, repo)
	if err != nil {
		return err
	}
	tok, err := a.InstallationToken(ctx, id)
	if err != nil {
		return err
	}
	_, err = a.api.request(ctx, tok, http.MethodGet, path, nil, out)
	if isUnauthorized(err) {
		a.forget(id)
	}
	return err
}

// IsNotFound reports whether err is a 404 from the API (e.g. BranchHead of
// a branch that does not exist).
func IsNotFound(err error) bool {
	var e *Error
	return errors.Is(err, ErrNotFound) || errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}

func isUnauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.StatusCode == http.StatusUnauthorized
}
