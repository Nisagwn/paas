package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
)

// GitHubLogin is the web UI's GitHub OAuth login. Who may sign in:
//
//   - logins in Admins (PAAS_ADMIN_GITHUB_LOGINS); they also become owners
//     of the "default" team on every sign-in (bootstrap),
//   - logins in AllowedUsers (PAAS_ALLOWED_GITHUB_USERS),
//   - members of an organization in AllowedOrgs (PAAS_ALLOWED_GITHUB_ORG),
//   - users a team owner added to a team (invitation),
//   - with OpenSignup, every GitHub account (a public platform): a new user
//     gets a personal team and sees only its own projects.
//
// Otherwise everyone else is refused.
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
	// OpenSignup lets any GitHub account sign in (PAAS_OPEN_SIGNUP).
	OpenSignup bool
	// MinAccountAge refuses open sign-ups of GitHub accounts younger than
	// this (Faz 23, PAAS_SIGNUP_MIN_ACCOUNT_AGE); 0 is off. Admins,
	// allowlisted and invited users are not affected.
	MinAccountAge time.Duration
	// Now is the clock of the account age check (tests); nil is time.Now.
	Now func() time.Time
	Log *slog.Logger
}

// ErrNotAllowed: the GitHub account is not on any allowlist.
var ErrNotAllowed = errors.New("this GitHub account is not allowed to sign in")

// ErrAccountTooNew: open sign-up refused a GitHub account younger than
// MinAccountAge (Faz 23).
var ErrAccountTooNew = errors.New("this GitHub account is too new to sign up")

// tooNew applies MinAccountAge; an account without a creation date passes.
func (g *GitHubLogin) tooNew(acct github.User) bool {
	if g.MinAccountAge <= 0 || acct.CreatedAt.IsZero() {
		return false
	}
	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}
	return now.Sub(acct.CreatedAt) < g.MinAccountAge
}

// invited reports whether the GitHub account already belongs to a team.
func invited(ctx context.Context, st *store.Store, githubID int64) (bool, error) {
	u, err := st.UserByGitHubID(ctx, githubID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return st.HasTeams(ctx, u.ID)
}

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
	allowed := admin || g.OpenSignup || hasFold(g.AllowedUsers, acct.Login)
	if !allowed && len(g.AllowedOrgs) > 0 {
		orgs, err := gh.UserOrgs(ctx)
		if err != nil {
			return store.User{}, fmt.Errorf("list organizations: %w", err)
		}
		allowed = slices.ContainsFunc(orgs, func(o string) bool { return hasFold(g.AllowedOrgs, o) })
	}
	if allowed && !admin && !hasFold(g.AllowedUsers, acct.Login) && g.tooNew(acct) {
		// Open sign-up only: an invited account may still come in.
		if ok, err := invited(ctx, st, acct.ID); err != nil {
			return store.User{}, err
		} else if !ok {
			return store.User{}, ErrAccountTooNew
		}
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
	// Faz 23: an admin suspended the account (admins of the platform
	// cannot lock themselves out).
	if suspended, err := st.UserSuspended(ctx, u.ID); err != nil {
		return u, err
	} else if suspended && !admin {
		return u, ErrSuspended
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
	if g.OpenSignup && !admin {
		if err := personalTeam(ctx, st, u); err != nil {
			return u, fmt.Errorf("personal team: %w", err)
		}
	}
	return u, nil
}

// personalTeam gives a user without any team one of their own, named after
// the GitHub login, with the user as owner.
func personalTeam(ctx context.Context, st *store.Store, u store.User) error {
	if ok, err := st.HasTeams(ctx, u.ID); err != nil || ok {
		return err
	}
	base := TeamSlug(u.Login)
	for i := 0; i < 20; i++ {
		slug := base
		if i > 0 {
			slug = fmt.Sprintf("%s-%d", base, i+1)
		}
		_, err := st.CreateTeam(ctx, slug, u.Login, u.ID)
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return errors.New("no free team name for " + u.Login)
}

// TeamSlug turns a GitHub login into a valid team slug
// (^[a-z][a-z0-9-]{1,30}$): lowercase, other characters dropped.
func TeamSlug(login string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(login) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "u-" + s
	}
	if len(s) > 28 { // room for a "-NN" suffix
		s = strings.TrimRight(s[:28], "-")
	}
	if len(s) < 2 {
		s += "-team"
	}
	return s
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
