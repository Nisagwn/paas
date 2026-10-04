package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nisagwn/paas/internal/store"
)

// Principal is the authenticated caller.
type Principal struct {
	// UserID is 0 for the admin principal.
	UserID int64
	Login  string
	Name   string
	Avatar string
	// Admin is the legacy PAAS_API_TOKEN (or a token login in dev mode):
	// owner of every team, but no user, so it has no personal tokens.
	Admin bool
}

// Admin is the principal of the legacy API token.
var Admin = Principal{Login: "admin", Admin: true}

func principalOf(u store.User) Principal {
	return Principal{UserID: u.ID, Login: u.Login, Name: u.Name, Avatar: u.AvatarURL}
}

type ctxKey struct{}

// WithPrincipal stores the caller in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// From returns the caller stored by WithPrincipal.
func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// ---- personal API tokens ----

// TokenPrefix starts every personal API token, so leaked tokens are easy to
// recognize (e.g. by secret scanners).
const TokenPrefix = "paas_"

// NewAPIToken returns a fresh token, its SHA-256 (hex, stored) and the
// prefix shown in listings. The plain token is shown to the user once.
func NewAPIToken() (plain, hash, display string) {
	b := make([]byte, 32)
	rand.Read(b)
	plain = TokenPrefix + b64.EncodeToString(b)
	return plain, HashToken(plain), plain[:len(TokenPrefix)+6]
}

// HashToken is the stored form of a token. Tokens carry 256 random bits, so
// a fast hash suffices: there is nothing to brute-force.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// ---- authenticator ----

// ErrUnauthenticated: no valid credentials.
var ErrUnauthenticated = errors.New("missing or invalid credentials")

// Authenticator resolves requests to principals and roles.
type Authenticator struct {
	Store *store.Store
	// Sessions also holds the legacy admin token, PAAS_API_TOKEN
	// (CheckToken); an empty token disables it.
	Sessions *Sessions
	// OAuth is set when GitHub login is configured. The token login form is
	// then disabled and admin sessions are refused.
	OAuth bool
	// Users resolves GitHub logins of members who never signed in; nil
	// limits new members to known users.
	Users UserDirectory
	Log   *slog.Logger
}

// UserDirectory looks up a GitHub account by login (github.Client).
type UserDirectory interface {
	LookupUser(ctx context.Context, login string) (store.User, error)
}

// TokenLogin reports whether the web UI offers the token login form.
func (a *Authenticator) TokenLogin() bool { return !a.OAuth && a.Sessions.HasToken() }

// Bearer resolves an Authorization bearer token: the legacy admin token or
// a personal "paas_" token.
func (a *Authenticator) Bearer(ctx context.Context, token string) (Principal, error) {
	if a.Sessions.CheckToken(token) {
		return Admin, nil
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		return Principal{}, ErrUnauthenticated
	}
	u, err := a.Store.UserByTokenHash(ctx, HashToken(token))
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrExpired) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	return principalOf(u), nil
}

// Session resolves r's session cookie. A session of a deleted user, or an
// admin session while GitHub login is configured, is invalid.
func (a *Authenticator) Session(r *http.Request) (Principal, error) {
	uid, ok := a.Sessions.User(r)
	switch {
	case !ok:
		return Principal{}, ErrUnauthenticated
	case uid == 0:
		if !a.TokenLogin() {
			return Principal{}, ErrUnauthenticated
		}
		return Admin, nil
	}
	u, err := a.Store.GetUser(r.Context(), uid)
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	return principalOf(u), nil
}

// TeamRole is p's role in a team ("" when not a member). The admin is an
// owner everywhere.
func (a *Authenticator) TeamRole(ctx context.Context, p Principal, teamID int64) (string, error) {
	if p.Admin {
		return store.RoleOwner, nil
	}
	return a.Store.TeamRole(ctx, p.UserID, teamID)
}

// Teams lists the teams p can see, with p's role.
func (a *Authenticator) Teams(ctx context.Context, p Principal) ([]store.Team, error) {
	if !p.Admin {
		return a.Store.TeamsForUser(ctx, p.UserID)
	}
	teams, err := a.Store.ListTeams(ctx)
	for i := range teams {
		teams[i].Role = store.RoleOwner
	}
	return teams, err
}

// Apps lists the apps p can see.
func (a *Authenticator) Apps(ctx context.Context, p Principal) ([]store.App, error) {
	if p.Admin {
		return a.Store.ListApps(ctx)
	}
	return a.Store.AppsForUser(ctx, p.UserID)
}

// DefaultTeam picks the team for a new app when none is named: the first
// team where p may create apps (member or owner); the "default" team for
// the admin. ErrNotFound when there is none.
func (a *Authenticator) DefaultTeam(ctx context.Context, p Principal) (store.Team, error) {
	if p.Admin {
		return a.Store.GetTeamBySlug(ctx, store.DefaultTeam)
	}
	teams, err := a.Store.TeamsForUser(ctx, p.UserID)
	if err != nil {
		return store.Team{}, err
	}
	for _, t := range teams {
		if store.RoleAllows(t.Role, store.RoleMember) {
			return t, nil
		}
	}
	return store.Team{}, store.ErrNotFound
}

// TeamForApps resolves a team slug for creating an app: ErrNotFound when p
// cannot see the team, ErrForbidden when p is only a viewer.
func (a *Authenticator) TeamForApps(ctx context.Context, p Principal, slug string) (store.Team, error) {
	if slug == "" {
		return a.DefaultTeam(ctx, p)
	}
	t, err := a.Store.GetTeamBySlug(ctx, slug)
	if err != nil {
		return t, err
	}
	role, err := a.TeamRole(ctx, p, t.ID)
	switch {
	case err != nil:
		return t, err
	case role == "":
		return t, store.ErrNotFound
	case !store.RoleAllows(role, store.RoleMember):
		return t, ErrForbidden
	}
	t.Role = role
	return t, nil
}

// ErrForbidden: the caller sees the resource but its role is too low.
var ErrForbidden = errors.New("insufficient role")

// ResolveLogin finds the user with a GitHub login, asking GitHub (and
// recording the account) when nobody with that login has signed in yet.
func (a *Authenticator) ResolveLogin(ctx context.Context, login string) (store.User, error) {
	u, err := a.Store.UserByLogin(ctx, login)
	if !errors.Is(err, store.ErrNotFound) || a.Users == nil {
		return u, err
	}
	gh, err := a.Users.LookupUser(ctx, login)
	if err != nil {
		return u, err
	}
	return a.Store.UpsertUser(ctx, gh.GitHubID, gh.Login, gh.Name, gh.AvatarURL)
}
