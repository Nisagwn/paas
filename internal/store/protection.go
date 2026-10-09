package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Faz 24: deployment protection (migration 017). The settings live here;
// internal/protect enforces them (Traefik ForwardAuth), internal/deploy
// attaches the middleware to the protected Ingresses.

// Protection modes and scopes (the CHECK constraints of app_protection).
const (
	ProtectOff      = "off"
	ProtectTeam     = "team"
	ProtectPassword = "password"

	ScopePreviews = "previews"
	ScopeAll      = "all"
)

// Protection is an app's protection setting. Apps without a row are
// unprotected: mode off, scope previews, version 0.
type Protection struct {
	AppID   int64  `json:"-"`
	AppName string `json:"-"`
	TeamID  int64  `json:"-"`
	Mode    string `json:"mode"`
	Scope   string `json:"scope"`
	// PasswordHash and BypassHash never leave the server.
	PasswordHash    string     `json:"-"`
	BypassHash      string     `json:"-"`
	BypassPrefix    string     `json:"-"`
	BypassCreatedAt *time.Time `json:"-"`
	Version         int64      `json:"-"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
}

// Enabled reports whether any protection is on.
func (p Protection) Enabled() bool { return p.Mode == ProtectTeam || p.Mode == ProtectPassword }

// Covers reports whether a route of the given kind is protected:
// deployment URLs and preview aliases with any scope, production aliases
// and custom domains only with scope "all". kind is an alias kind
// (AliasProduction, AliasPreview, AliasCustom) or "" for a deployment URL.
func (p Protection) Covers(kind string) bool {
	if !p.Enabled() {
		return false
	}
	switch kind {
	case AliasProduction, AliasCustom:
		return p.Scope == ScopeAll
	}
	return true
}

func defaultProtection(app App) Protection {
	return Protection{AppID: app.ID, AppName: app.Name, TeamID: app.TeamID, Mode: ProtectOff, Scope: ScopePreviews}
}

const protectionCols = `a.id, a.name, a.team_id, COALESCE(p.mode, 'off'), COALESCE(p.scope, 'previews'),
	COALESCE(p.password_hash, ''), COALESCE(p.bypass_hash, ''), COALESCE(p.bypass_prefix, ''), p.bypass_created_at,
	COALESCE(p.version, 0), p.updated_at`

func scanProtection(row interface{ Scan(...any) error }) (Protection, error) {
	var p Protection
	err := row.Scan(&p.AppID, &p.AppName, &p.TeamID, &p.Mode, &p.Scope, &p.PasswordHash, &p.BypassHash,
		&p.BypassPrefix, &p.BypassCreatedAt, &p.Version, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// GetProtection returns the app's setting (the default when none is stored).
func (s *Store) GetProtection(ctx context.Context, appID int64) (Protection, error) {
	return scanProtection(s.db.QueryRowContext(ctx, `SELECT `+protectionCols+`
		FROM apps a LEFT JOIN app_protection p ON p.app_id = a.id WHERE a.id = $1`, appID))
}

// ProtectionByAppName is GetProtection by app name; ErrNotFound when the
// app does not exist. deploy.Kubernetes and the ForwardAuth endpoint use it.
func (s *Store) ProtectionByAppName(ctx context.Context, name string) (Protection, error) {
	return scanProtection(s.db.QueryRowContext(ctx, `SELECT `+protectionCols+`
		FROM apps a LEFT JOIN app_protection p ON p.app_id = a.id WHERE a.name = $1`, name))
}

// SetProtection stores mode and scope. passwordHash replaces the stored
// password when non-empty. The version grows when the mode or the password
// changes, which invalidates every access cookie of the app. Password mode
// without any password is ErrInvalid.
func (s *Store) SetProtection(ctx context.Context, appID int64, mode, scope, passwordHash string) (Protection, error) {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO app_protection (app_id, mode, scope, password_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (app_id) DO UPDATE SET
			mode = EXCLUDED.mode, scope = EXCLUDED.scope,
			password_hash = CASE WHEN $4 <> '' THEN $4 ELSE app_protection.password_hash END,
			version = app_protection.version +
				CASE WHEN app_protection.mode <> EXCLUDED.mode OR $4 <> '' THEN 1 ELSE 0 END,
			updated_at = now()`, appID, mode, scope, passwordHash)
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23514" { // check_violation
		return Protection{}, ErrInvalid
	}
	if err != nil {
		return Protection{}, err
	}
	return s.GetProtection(ctx, appID)
}

// SetProtectionBypass stores a new bypass secret (its SHA-256); "" removes it.
func (s *Store) SetProtectionBypass(ctx context.Context, appID int64, hash, prefix string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO app_protection (app_id, bypass_hash, bypass_prefix, bypass_created_at)
		VALUES ($1, $2, $3, CASE WHEN $2 = '' THEN NULL ELSE now() END)
		ON CONFLICT (app_id) DO UPDATE SET bypass_hash = EXCLUDED.bypass_hash,
			bypass_prefix = EXCLUDED.bypass_prefix, bypass_created_at = EXCLUDED.bypass_created_at,
			updated_at = now()`, appID, hash, prefix)
	return err
}

// ShareLink is a shareable link of a protected app. The token itself is
// shown once; Prefix tells links apart.
type ShareLink struct {
	ID         int64      `json:"id"`
	AppID      int64      `json:"-"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedBy  string     `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// Expired reports whether the link no longer grants access at now.
func (l ShareLink) Expired(now time.Time) bool {
	return l.ExpiresAt != nil && !now.Before(*l.ExpiresAt)
}

const shareCols = `l.id, l.app_id, l.name, l.prefix, COALESCE(u.login, ''), l.created_at, l.expires_at, l.last_used_at`

func scanShare(row interface{ Scan(...any) error }) (ShareLink, error) {
	var l ShareLink
	err := row.Scan(&l.ID, &l.AppID, &l.Name, &l.Prefix, &l.CreatedBy, &l.CreatedAt, &l.ExpiresAt, &l.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// CreateShareLink stores a link; only the token's SHA-256 is kept.
// createdBy 0 means the admin token.
func (s *Store) CreateShareLink(ctx context.Context, appID int64, name, tokenHash, prefix string, createdBy int64, expires *time.Time) (ShareLink, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO protection_share_links (app_id, name, token_hash, prefix, created_by, expires_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, 0), $6) RETURNING id`,
		appID, name, tokenHash, prefix, createdBy, expires).Scan(&id)
	if isUniqueViolation(err) {
		return ShareLink{}, ErrConflict
	}
	if err != nil {
		return ShareLink{}, err
	}
	return s.GetShareLink(ctx, appID, id)
}

// GetShareLink loads one of the app's links (expired ones included).
func (s *Store) GetShareLink(ctx context.Context, appID, id int64) (ShareLink, error) {
	return scanShare(s.db.QueryRowContext(ctx, `SELECT `+shareCols+`
		FROM protection_share_links l LEFT JOIN users u ON u.id = l.created_by
		WHERE l.app_id = $1 AND l.id = $2`, appID, id))
}

// ListShareLinks returns the app's links, newest first.
func (s *Store) ListShareLinks(ctx context.Context, appID int64) ([]ShareLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+shareCols+`
		FROM protection_share_links l LEFT JOIN users u ON u.id = l.created_by
		WHERE l.app_id = $1 ORDER BY l.id DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShareLink{}
	for rows.Next() {
		l, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteShareLink revokes one of the app's links.
func (s *Store) DeleteShareLink(ctx context.Context, appID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM protection_share_links WHERE id = $1 AND app_id = $2`, id, appID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UseShareLink finds the app's link with tokenHash and records its use.
// ErrNotFound for unknown tokens and tokens of other apps, ErrExpired for
// expired links.
func (s *Store) UseShareLink(ctx context.Context, appID int64, tokenHash string) (ShareLink, error) {
	var id int64
	var expires *time.Time
	err := s.db.QueryRowContext(ctx, `
		UPDATE protection_share_links SET last_used_at = now()
		WHERE app_id = $1 AND token_hash = $2 RETURNING id, expires_at`, appID, tokenHash).Scan(&id, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ShareLink{}, ErrNotFound
	}
	if err != nil {
		return ShareLink{}, err
	}
	l, err := s.GetShareLink(ctx, appID, id)
	if err == nil && l.Expired(time.Now()) {
		return l, ErrExpired
	}
	return l, err
}

// UseNonce records a one-time token as redeemed; false when it already
// was. Expired rows are dropped on the way.
func (s *Store) UseNonce(ctx context.Context, nonce string, expires time.Time) (bool, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM protection_nonces WHERE expires_at < now()`); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO protection_nonces (nonce, expires_at) VALUES ($1, $2)
		ON CONFLICT (nonce) DO NOTHING`, nonce, expires)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// AppOwnsHost reports whether host (lowercase, without a port) is one of
// the app's addresses: an alias, a custom domain, or the URL of one of its
// deployments (<sha7>[-<n>]-<app>.<domain>).
func (s *Store) AppOwnsHost(ctx context.Context, app App, host, domain string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM aliases WHERE app_id = $1 AND hostname = $2)
		    OR EXISTS (SELECT 1 FROM app_domains WHERE app_id = $1 AND hostname = $2)`, app.ID, host).Scan(&ok)
	if err != nil || ok {
		return ok, err
	}
	key, found := strings.CutSuffix(host, "-"+app.Name+"."+domain)
	if !found || domain == "" {
		return false, nil
	}
	sha7, gen := key, 0
	if i := strings.IndexByte(key, '-'); i >= 0 {
		n, err := strconv.Atoi(key[i+1:])
		if err != nil || n <= 0 {
			return false, nil
		}
		sha7, gen = key[:i], n
	}
	if len(sha7) != 7 || strings.Trim(sha7, "0123456789abcdef") != "" {
		return false, nil
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM deployments
			WHERE app_id = $1 AND left(commit_sha, 7) = $2 AND generation = $3)`, app.ID, sha7, gen).Scan(&ok)
	return ok, err
}
