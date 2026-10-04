package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
)

// GitHubLogin is the web UI's GitHub OAuth login. Who may sign in:
//
//   - logins in Admins (PAAS_ADMIN_GITHUB_LOGINS); they also become owners
//     of the "default" team on every sign-in (bootstrap),
//   - logins in AllowedUsers (PAAS_ALLOWED_GITHUB_USERS),
//   - members of an organization in AllowedOrgs (PAAS_ALLOWED_GITHUB_ORG),
//   - users a team owner added to a team (invitation).
//
// Everyone else is refused: an arbitrary GitHub account never gets in.
type GitHubLogin struct {
	App *github.OAuthApp
	// APIURL is the REST API used with the user's token.
	APIURL string
	// RedirectURI is <PAAS_PUBLIC_URL>/auth/github/callback; it must match
	// the OAuth App's callback URL.
	RedirectURI  string
	Admins       []string
	AllowedUsers []string
	AllowedOrgs  []string
	Log          *slog.Logger
}

// ErrNotAllowed: the GitHub account is not on any allowlist.
var ErrNotAllowed = errors.New("this GitHub account is not allowed to sign in")

// Scope asks for read:org only when organization membership is checked.
func (g *GitHubLogin) Scope() string {
	if len(g.AllowedOrgs) > 0 {
		return "read:org"
	}
	return ""
}

// PKCEChallenge is the S256 code challenge of a verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64.EncodeToString(sum[:])
}

func hasFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(v string) bool { return strings.EqualFold(v, s) })
}

// Complete finishes a login: it exchanges the code, identifies the GitHub
// account, applies the allowlists, records the user and bootstraps admins.
// The GitHub access token is used for these calls only and never stored.
func (g *GitHubLogin) Complete(ctx context.Context, st *store.Store, code, verifier string) (store.User, error) {
	token, err := g.App.Exchange(ctx, code, g.RedirectURI, verifier)
	if err != nil {
		return store.User{}, err
	}
	gh := github.New(g.APIURL, token, g.Log)
	acct, err := gh.AuthenticatedUser(ctx)
	if err != nil {
		return store.User{}, err
	}
	if acct.ID == 0 || acct.Login == "" {
		return store.User{}, errors.New("github: /user returned no account")
	}
	admin := hasFold(g.Admins, acct.Login)
	allowed := admin || hasFold(g.AllowedUsers, acct.Login)
	if !allowed && len(g.AllowedOrgs) > 0 {
		orgs, err := gh.UserOrgs(ctx)
		if err != nil {
			return store.User{}, fmt.Errorf("list organizations: %w", err)
		}
		allowed = slices.ContainsFunc(orgs, func(o string) bool { return hasFold(g.AllowedOrgs, o) })
	}
	if !allowed {
		// Invited by a team owner?
		u, err := st.UserByGitHubID(ctx, acct.ID)
		if errors.Is(err, store.ErrNotFound) {
			return store.User{}, ErrNotAllowed
		}
		if err != nil {
			return store.User{}, err
		}
		if ok, err := st.HasTeams(ctx, u.ID); err != nil {
			return store.User{}, err
		} else if !ok {
			return store.User{}, ErrNotAllowed
		}
	}
	u, err := st.UpsertUser(ctx, acct.ID, acct.Login, acct.Name, acct.AvatarURL)
	if err != nil {
		return u, err
	}
	if admin {
		t, err := st.GetTeamBySlug(ctx, store.DefaultTeam)
		if err == nil {
			err = st.SetMember(ctx, t.ID, u.ID, store.RoleOwner)
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return u, fmt.Errorf("bootstrap admin: %w", err)
		}
	}
	return u, nil
}

// GitHubDirectory resolves logins through the GitHub API (UserDirectory).
type GitHubDirectory struct{ Client *github.Client }

func (d GitHubDirectory) LookupUser(ctx context.Context, login string) (store.User, error) {
	u, err := d.Client.GetUser(ctx, login)
	if errors.Is(err, github.ErrNotFound) {
		return store.User{}, store.ErrNotFound
	}
	if err != nil {
		return store.User{}, err
	}
	return store.User{GitHubID: u.ID, Login: u.Login, Name: u.Name, AvatarURL: u.AvatarURL}, nil
}
