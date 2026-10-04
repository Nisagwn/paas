package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

// Team and token operations shared by the API and the web UI, with their
// authorization rules. Errors: store.ErrNotFound (team not visible, unknown
// user), ErrForbidden, store.ErrConflict, store.ErrLastOwner, or an
// *InvalidError for bad input.

// InvalidError is rejected input; its message is safe to show.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(msg string) error { return &InvalidError{msg} }

var githubLoginRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// Team returns a team p can see, with p's role.
func (a *Authenticator) Team(ctx context.Context, p Principal, slug string) (store.Team, error) {
	t, err := a.Store.GetTeamBySlug(ctx, slug)
	if err != nil {
		return t, err
	}
	role, err := a.TeamRole(ctx, p, t.ID)
	if err != nil {
		return t, err
	}
	if role == "" {
		return t, store.ErrNotFound
	}
	t.Role = role
	return t, nil
}

// CreateTeam creates a team; p becomes its owner (the admin creates teams
// without members and adds owners afterwards).
func (a *Authenticator) CreateTeam(ctx context.Context, p Principal, slug, name string) (store.Team, error) {
	slug, name = strings.TrimSpace(slug), strings.TrimSpace(name)
	if !store.ValidTeamSlug(slug) {
		return store.Team{}, invalid("team slug must be 2-31 chars: lowercase letters, digits, '-', starting with a letter")
	}
	if name == "" {
		name = slug
	}
	if len(name) > 100 {
		return store.Team{}, invalid("team name is longer than 100 characters")
	}
	return a.Store.CreateTeam(ctx, slug, name, p.UserID)
}

// SetMember adds the GitHub user login to the team with role, or changes
// their role. Owners only.
func (a *Authenticator) SetMember(ctx context.Context, p Principal, slug, login, role string) (store.User, error) {
	if !store.ValidRole(role) {
		return store.User{}, invalid("role must be owner, member or viewer")
	}
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	if !githubLoginRe.MatchString(login) {
		return store.User{}, invalid("invalid GitHub login")
	}
	t, err := a.Team(ctx, p, slug)
	if err != nil {
		return store.User{}, err
	}
	if t.Role != store.RoleOwner {
		return store.User{}, ErrForbidden
	}
	u, err := a.ResolveLogin(ctx, login)
	if err != nil {
		return u, err
	}
	return u, a.Store.SetMember(ctx, t.ID, u.ID, role)
}

// RemoveMember removes login from the team. Owners may remove anyone;
// everybody may leave a team.
func (a *Authenticator) RemoveMember(ctx context.Context, p Principal, slug, login string) error {
	t, err := a.Team(ctx, p, slug)
	if err != nil {
		return err
	}
	u, err := a.Store.UserByLogin(ctx, login)
	if err != nil {
		return err
	}
	if t.Role != store.RoleOwner && u.ID != p.UserID {
		return ErrForbidden
	}
	return a.Store.RemoveMember(ctx, t.ID, u.ID)
}

// MaxTokenDays bounds a token's lifetime when one is given.
const MaxTokenDays = 3650

// CreateToken issues a personal API token for p; plain is shown once.
// expiresInDays 0 means no expiry.
func (a *Authenticator) CreateToken(ctx context.Context, p Principal, name string, expiresInDays int) (store.APIToken, string, error) {
	if p.UserID == 0 {
		return store.APIToken{}, "", invalid("personal tokens belong to a user; sign in with GitHub")
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return store.APIToken{}, "", invalid("token name must be 1-100 characters")
	}
	if expiresInDays < 0 || expiresInDays > MaxTokenDays {
		return store.APIToken{}, "", invalid("expires_in_days must be between 0 (never) and 3650")
	}
	var exp *time.Time
	if expiresInDays > 0 {
		t := time.Now().Add(time.Duration(expiresInDays) * 24 * time.Hour)
		exp = &t
	}
	plain, hash, prefix := NewAPIToken()
	t, err := a.Store.CreateAPIToken(ctx, p.UserID, name, hash, prefix, exp)
	return t, plain, err
}

// IsInvalid reports whether err is rejected input, and its message.
func IsInvalid(err error) (string, bool) {
	var e *InvalidError
	if errors.As(err, &e) {
		return e.Msg, true
	}
	return "", false
}
