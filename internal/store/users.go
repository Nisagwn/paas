package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Faz 13: users, teams, memberships and personal API tokens.

// Team roles, from most to least privileged. They mirror the CHECK
// constraint on team_members.role.
const (
	RoleOwner  = "owner"
	RoleMember = "member"
	RoleViewer = "viewer"
)

// DefaultTeam holds the apps created before teams existed and apps created
// without a team (admin token, webhook-era scripts).
const DefaultTeam = "default"

var (
	// ErrLastOwner: the change would leave a team without an owner.
	ErrLastOwner = errors.New("a team needs at least one owner")
	// ErrExpired: the API token exists but has expired.
	ErrExpired = errors.New("token expired")
)

var teamSlugRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

// ValidTeamSlug mirrors the CHECK constraint on teams.slug.
func ValidTeamSlug(s string) bool { return teamSlugRe.MatchString(s) }

// ValidRole reports whether r is one of the three team roles.
func ValidRole(r string) bool { return r == RoleOwner || r == RoleMember || r == RoleViewer }

// RoleRank orders roles: owner 3, member 2, viewer 1, anything else 0.
func RoleRank(r string) int {
	switch r {
	case RoleOwner:
		return 3
	case RoleMember:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// RoleAllows reports whether role have includes the permissions of need.
func RoleAllows(have, need string) bool {
	return RoleRank(have) > 0 && RoleRank(have) >= RoleRank(need)
}

type User struct {
	ID        int64     `json:"id"`
	GitHubID  int64     `json:"github_id"`
	Login     string    `json:"login"`
	Name      string    `json:"name"`
	AvatarURL string    `json:"avatar_url"`
	CreatedAt time.Time `json:"created_at"`
}

type Team struct {
	ID        int64     `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Role is the caller's role when listed for a user; empty otherwise.
	Role string `json:"role,omitempty"`
}

type Member struct {
	User
	Role string `json:"role"`
}

type APIToken struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// ---- users ----

const userCols = `id, github_id, login, name, avatar_url, created_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.GitHubID, &u.Login, &u.Name, &u.AvatarURL, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// UpsertUser creates or refreshes the user with GitHub account id githubID.
// A login is unique at any time, but GitHub lets users rename and others
// claim a freed name: a stale row holding the login gets a placeholder
// until that account signs in again.
func (s *Store) UpsertUser(ctx context.Context, githubID int64, login, name, avatarURL string) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET login = login || '~' || github_id::text
		WHERE lower(login) = lower($1) AND github_id <> $2`, login, githubID); err != nil {
		return User{}, err
	}
	u, err := scanUser(tx.QueryRowContext(ctx, `
		INSERT INTO users (github_id, login, name, avatar_url) VALUES ($1, $2, $3, $4)
		ON CONFLICT (github_id) DO UPDATE SET login = EXCLUDED.login, name = EXCLUDED.name,
			avatar_url = EXCLUDED.avatar_url
		RETURNING `+userCols, githubID, login, name, avatarURL))
	if err != nil {
		return u, err
	}
	return u, tx.Commit()
}

func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

// UserByLogin matches a GitHub login case-insensitively.
func (s *Store) UserByLogin(ctx context.Context, login string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE lower(login) = lower($1)`, login))
}

func (s *Store) UserByGitHubID(ctx context.Context, githubID int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE github_id = $1`, githubID))
}

// HasTeams reports whether the user belongs to at least one team, i.e. was
// invited by a team owner.
func (s *Store) HasTeams(ctx context.Context, userID int64) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM team_members WHERE user_id = $1)`, userID).Scan(&ok)
	return ok, err
}

// ---- teams ----

const teamCols = `t.id, t.slug, t.name, t.created_at`

func scanTeam(row interface{ Scan(...any) error }, extra ...any) (Team, error) {
	var t Team
	err := row.Scan(append([]any{&t.ID, &t.Slug, &t.Name, &t.CreatedAt}, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// CreateTeam creates a team; ownerID (0: none) becomes its first owner.
func (s *Store) CreateTeam(ctx context.Context, slug, name string, ownerID int64) (Team, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Team{}, err
	}
	defer tx.Rollback()
	t, err := scanTeam(tx.QueryRowContext(ctx,
		`INSERT INTO teams AS t (slug, name) VALUES ($1, $2) RETURNING `+teamCols, slug, name))
	if isUniqueViolation(err) {
		return t, ErrConflict
	}
	if err != nil {
		return t, err
	}
	if ownerID != 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, $3)`,
			t.ID, ownerID, RoleOwner); err != nil {
			return t, err
		}
		t.Role = RoleOwner
	}
	return t, tx.Commit()
}

func (s *Store) GetTeamBySlug(ctx context.Context, slug string) (Team, error) {
	return scanTeam(s.db.QueryRowContext(ctx, `SELECT `+teamCols+` FROM teams t WHERE slug = $1`, slug))
}

func (s *Store) GetTeam(ctx context.Context, id int64) (Team, error) {
	return scanTeam(s.db.QueryRowContext(ctx, `SELECT `+teamCols+` FROM teams t WHERE id = $1`, id))
}

// ListTeams returns every team (admin view).
func (s *Store) ListTeams(ctx context.Context) ([]Team, error) {
	return s.listTeams(ctx, `SELECT `+teamCols+`, '' FROM teams t ORDER BY t.id`)
}

// TeamsForUser returns the user's teams, oldest first, with the user's role.
func (s *Store) TeamsForUser(ctx context.Context, userID int64) ([]Team, error) {
	return s.listTeams(ctx, `SELECT `+teamCols+`, m.role FROM teams t
		JOIN team_members m ON m.team_id = t.id WHERE m.user_id = $1 ORDER BY t.id`, userID)
}

func (s *Store) listTeams(ctx context.Context, query string, args ...any) ([]Team, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Team{}
	for rows.Next() {
		var role string
		t, err := scanTeam(rows, &role)
		if err != nil {
			return nil, err
		}
		t.Role = role
		out = append(out, t)
	}
	return out, rows.Err()
}

// TeamRole returns the user's role in the team, or "" when not a member.
func (s *Store) TeamRole(ctx context.Context, userID, teamID int64) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT role FROM team_members WHERE team_id = $1 AND user_id = $2`, teamID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// TeamMembers lists a team's members, owners first.
func (s *Store) TeamMembers(ctx context.Context, teamID int64) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.github_id, u.login, u.name, u.avatar_url, u.created_at, m.role
		FROM team_members m JOIN users u ON u.id = m.user_id WHERE m.team_id = $1
		ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'member' THEN 1 ELSE 2 END, lower(u.login)`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.GitHubID, &m.Login, &m.Name, &m.AvatarURL, &m.CreatedAt, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMember adds a user to a team or changes their role. Demoting the last
// owner fails with ErrLastOwner.
func (s *Store) SetMember(ctx context.Context, teamID, userID int64, role string) error {
	if !ValidRole(role) {
		return errors.New("invalid role " + role)
	}
	return s.changeMembers(ctx, teamID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, $3)
			ON CONFLICT (team_id, user_id) DO UPDATE SET role = EXCLUDED.role`, teamID, userID, role)
		return err
	})
}

// RemoveMember removes a user from a team (ErrNotFound when not a member).
// Removing the last owner fails with ErrLastOwner.
func (s *Store) RemoveMember(ctx context.Context, teamID, userID int64) error {
	return s.changeMembers(ctx, teamID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, teamID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// changeMembers runs change with the team row locked, so concurrent changes
// cannot both remove "the other" owner, and then checks an owner remains.
// A team that had no owner (created by the admin token) may stay without.
func (s *Store) changeMembers(ctx context.Context, teamID int64, change func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT id FROM teams WHERE id = $1 FOR UPDATE`, teamID).Scan(&teamID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	owners := `SELECT count(*) FROM team_members WHERE team_id = $1 AND role = 'owner'`
	var before, after int
	if err := tx.QueryRowContext(ctx, owners, teamID).Scan(&before); err != nil {
		return err
	}
	if err := change(tx); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, owners, teamID).Scan(&after); err != nil {
		return err
	}
	if before > 0 && after == 0 {
		return ErrLastOwner
	}
	return tx.Commit()
}

// ---- apps by team ----

// AppsForUser lists the apps of every team the user belongs to.
func (s *Store) AppsForUser(ctx context.Context, userID int64) ([]App, error) {
	return s.listApps(ctx, `SELECT `+appCols+` FROM apps
		WHERE team_id IN (SELECT team_id FROM team_members WHERE user_id = $1) ORDER BY name`, userID)
}

// ---- API tokens ----

const tokenCols = `id, user_id, name, prefix, created_at, last_used_at, expires_at`

func scanToken(row interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// CreateAPIToken stores a token by its SHA-256 (hex); the plain token is
// never stored. expiresAt nil means no expiry.
func (s *Store) CreateAPIToken(ctx context.Context, userID int64, name, hash, prefix string, expiresAt *time.Time) (APIToken, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `
		INSERT INTO api_tokens (user_id, name, token_hash, prefix, expires_at) VALUES ($1, $2, $3, $4, $5)
		RETURNING `+tokenCols, userID, strings.TrimSpace(name), hash, prefix, expiresAt))
	if isUniqueViolation(err) {
		return t, ErrConflict
	}
	return t, err
}

// APITokens lists a user's tokens, newest first.
func (s *Store) APITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE user_id = $1 ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken deletes one of the user's tokens; another user's token id
// answers ErrNotFound.
func (s *Store) RevokeAPIToken(ctx context.Context, userID, tokenID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = $1 AND user_id = $2`, tokenID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UserByTokenHash resolves an API token to its user and records the use
// (at most once a minute, to spare writes). Unknown tokens answer
// ErrNotFound, expired ones ErrExpired.
func (s *Store) UserByTokenHash(ctx context.Context, hash string) (User, error) {
	var (
		tokenID int64
		expires *time.Time
		expired bool
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, expires_at, expires_at IS NOT NULL AND expires_at <= now()
		FROM api_tokens WHERE token_hash = $1`, hash).Scan(&tokenID, &expires, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	if expired {
		return User{}, ErrExpired
	}
	u, err := scanUser(s.db.QueryRowContext(ctx, `
		WITH touched AS (
			UPDATE api_tokens SET last_used_at = now()
			WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')
		)
		SELECT `+userCols+` FROM users WHERE id = (SELECT user_id FROM api_tokens WHERE id = $1)`, tokenID))
	return u, err
}
