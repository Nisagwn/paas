package protect

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
)

// Settings operations shared by the JSON API and the web UI. Rejected
// input is an *auth.InvalidError (auth.IsInvalid) with an English message;
// the web UI translates it.

const (
	MinPassword = 8
	MaxPassword = 256
	// MaxShareHours bounds a shareable link's lifetime when one is given.
	MaxShareHours = 24 * 365
)

func invalid(msg string) error { return &auth.InvalidError{Msg: msg} }

// Save validates and stores the setting. Empty mode or scope keeps the
// stored value; a non-empty password replaces the stored one (and signs
// every password visitor out).
func Save(ctx context.Context, st *store.Store, app store.App, mode, scope, password string) (store.Protection, error) {
	cur, err := st.GetProtection(ctx, app.ID)
	if err != nil {
		return cur, err
	}
	mode, scope = strings.TrimSpace(mode), strings.TrimSpace(scope)
	if mode == "" {
		mode = cur.Mode
	}
	if scope == "" {
		scope = cur.Scope
	}
	switch mode {
	case store.ProtectOff, store.ProtectTeam, store.ProtectPassword:
	default:
		return cur, invalid(`mode must be "off", "team" or "password"`)
	}
	if scope != store.ScopePreviews && scope != store.ScopeAll {
		return cur, invalid(`scope must be "previews" or "all"`)
	}
	hash := ""
	if password != "" {
		if n := utf8.RuneCountInString(password); n < MinPassword || n > MaxPassword || !utf8.ValidString(password) {
			return cur, invalid("password must be 8-256 characters")
		}
		if hash, err = HashPassword(password); err != nil {
			return cur, err
		}
	}
	if mode == store.ProtectPassword && hash == "" && cur.PasswordHash == "" {
		return cur, invalid("password protection needs a password")
	}
	p, err := st.SetProtection(ctx, app.ID, mode, scope, hash)
	if errors.Is(err, store.ErrInvalid) {
		return cur, invalid("password protection needs a password")
	}
	return p, err
}

// CreateShare stores a new shareable link and returns its token, which is
// never available again. expiresInHours 0 means no expiry; createdBy 0 is
// the admin token.
func CreateShare(ctx context.Context, st *store.Store, app store.App, name string, expiresInHours int, createdBy int64) (store.ShareLink, string, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > 100 {
		return store.ShareLink{}, "", invalid("share link name must be at most 100 characters")
	}
	if expiresInHours < 0 || expiresInHours > MaxShareHours {
		return store.ShareLink{}, "", invalid("expires_in_hours must be between 0 (never) and 8760")
	}
	var exp *time.Time
	if expiresInHours > 0 {
		t := time.Now().Add(time.Duration(expiresInHours) * time.Hour)
		exp = &t
	}
	plain, hash, prefix := NewShareToken()
	l, err := st.CreateShareLink(ctx, app.ID, name, hash, prefix, createdBy, exp)
	return l, plain, err
}

// ShareURL appends the link's token to u (an address of the app).
func ShareURL(u *url.URL, token string) string {
	c := *u
	q := c.Query()
	q.Set(ShareParam, token)
	c.RawQuery = q.Encode()
	return c.String()
}

// RotateBypass stores a new bypass secret and returns it; the previous one
// stops working.
func RotateBypass(ctx context.Context, st *store.Store, app store.App) (string, error) {
	plain, hash, prefix := NewBypassSecret()
	return plain, st.SetProtectionBypass(ctx, app.ID, hash, prefix)
}

// RemoveBypass deletes the bypass secret.
func RemoveBypass(ctx context.Context, st *store.Store, app store.App) error {
	return st.SetProtectionBypass(ctx, app.ID, "", "")
}

// AppURL parses raw as an absolute http(s) URL on one of the app's
// addresses (store.AppOwnsHost). It is how shareable links and login
// redirects are kept on the app's own hosts.
func AppURL(ctx context.Context, st *store.Store, app store.App, domain, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, invalid("url must be an http(s) address of this app")
	}
	ok, err := st.AppOwnsHost(ctx, app, strings.ToLower(u.Hostname()), domain)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, invalid("url must be an http(s) address of this app")
	}
	u.Fragment = ""
	return u, nil
}
