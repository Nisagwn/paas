package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 15: proving that a platform user can access a GitHub App installation
// before their team claims it. Knowing an installation id proves nothing
// (ids are sequential and appear in URLs), so the claim needs a fresh
// user-to-server token of the user's own GitHub account: GitHub lists an
// installation under GET /user/installations only for accounts that may
// manage or use it.

var (
	// ErrWrongAccount: the GitHub account that authorized is not the
	// signed-in platform user's.
	ErrWrongAccount = errors.New("authorized GitHub account differs from the signed-in user")
	// ErrNoInstallationAccess: the user's GitHub account cannot see the
	// installation.
	ErrNoInstallationAccess = errors.New("GitHub account cannot access this installation")
)

// InstallationGrant is what a user token shows of one installation.
type InstallationGrant struct {
	Installation store.Installation
	// Repos are the installation's repositories the user can see; they
	// seed the tables when the installation webhook has not arrived yet.
	Repos []store.InstallationRepo
}

// maxPages bounds paginated listings (100 per page).
const maxPages = 10

// VerifyInstallation exchanges an authorization code (PKCE) for a user
// token and checks that it belongs to the GitHub account githubID and that
// this account can access installation id. The token is used for these
// calls only and never stored.
func (g *GitHubLogin) VerifyInstallation(ctx context.Context, code, verifier string, githubID, id int64) (InstallationGrant, error) {
	var out InstallationGrant
	token, err := g.App.Exchange(ctx, code, g.RedirectURI, verifier)
	if err != nil {
		return out, err
	}
	acct, err := github.New(g.APIURL, token, g.Log).AuthenticatedUser(ctx)
	if err != nil {
		return out, err
	}
	if acct.ID == 0 || acct.ID != githubID {
		return out, ErrWrongAccount
	}

	api := userAPI{base: strings.TrimRight(g.apiURL(), "/"), token: token}
	found := false
	for page := 1; page <= maxPages && !found; page++ {
		var body struct {
			Installations []installationJSON `json:"installations"`
		}
		if err := api.get(ctx, "/user/installations?per_page=100&page="+strconv.Itoa(page), &body); err != nil {
			return out, err
		}
		for _, in := range body.Installations {
			if in.ID == id {
				out.Installation = in.store()
				found = true
			}
		}
		if len(body.Installations) < 100 {
			break
		}
	}
	if !found {
		return out, ErrNoInstallationAccess
	}
	for page := 1; page <= maxPages; page++ {
		var body struct {
			Repositories []struct {
				ID       int64  `json:"id"`
				FullName string `json:"full_name"`
				Private  bool   `json:"private"`
			} `json:"repositories"`
		}
		path := fmt.Sprintf("/user/installations/%d/repositories?per_page=100&page=%d", id, page)
		if err := api.get(ctx, path, &body); err != nil {
			return out, err
		}
		for _, r := range body.Repositories {
			out.Repos = append(out.Repos, store.InstallationRepo{InstallationID: id, RepoID: r.ID, FullName: r.FullName, Private: r.Private})
		}
		if len(body.Repositories) < 100 {
			break
		}
	}
	return out, nil
}

func (g *GitHubLogin) apiURL() string {
	if g.APIURL == "" {
		return github.DefaultBaseURL
	}
	return g.APIURL
}

type installationJSON struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	SuspendedAt *time.Time `json:"suspended_at"`
}

func (in installationJSON) store() store.Installation {
	typ := in.Account.Type
	if typ != "User" {
		typ = "Organization"
	}
	return store.Installation{ID: in.ID, AccountLogin: in.Account.Login, AccountType: typ, Suspended: in.SuspendedAt != nil}
}

// userAPI makes GET calls with a user-to-server token.
type userAPI struct {
	base, token string
}

func (u userAPI) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "paas")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("github: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github: GET %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}
