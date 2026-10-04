package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/lib/pq"
)

// Faz 15: GitHub App installations and the repositories they grant.

// Installation is one installation of the platform's GitHub App.
type Installation struct {
	ID           int64  `json:"id"`
	AccountLogin string `json:"account_login"`
	AccountType  string `json:"account_type"` // User | Organization
	TeamID       *int64 `json:"team_id,omitempty"`
	Suspended    bool   `json:"suspended"`
}

// InstallationRepo is a repository an installation grants access to.
type InstallationRepo struct {
	InstallationID int64  `json:"installation_id"`
	RepoID         int64  `json:"repo_id"`
	FullName       string `json:"full_name"`
	Private        bool   `json:"private"`
}

// ImportableRepo is a granted repository as the import page lists it.
type ImportableRepo struct {
	InstallationRepo
	AccountLogin string `json:"account_login"`
	// App is the name of the app already deployed from this repository, if any.
	App string `json:"app,omitempty"`
}

// UpsertInstallation records an installation (created, or seen again). It
// keeps the team that claimed it and clears suspended unless set.
func (s *Store) UpsertInstallation(ctx context.Context, in Installation) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO github_installations (id, account_login, account_type, suspended)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET account_login = EXCLUDED.account_login,
			account_type = EXCLUDED.account_type, suspended = EXCLUDED.suspended, updated_at = now()`,
		in.ID, in.AccountLogin, in.AccountType, in.Suspended)
	return err
}

// DeleteInstallation forgets an uninstalled installation and its repositories.
// Apps deployed from them stay; their next build fails until access returns.
func (s *Store) DeleteInstallation(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM github_installations WHERE id = $1`, id)
	return err
}

// SetInstallationSuspended records a suspend / unsuspend event.
func (s *Store) SetInstallationSuspended(ctx context.Context, id int64, suspended bool) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE github_installations SET suspended = $2, updated_at = now() WHERE id = $1`, id, suspended)
	return affected(res, err)
}

// ClaimInstallation assigns an installation to a team (the setup URL after
// "Install"). An installation belongs to one team; claiming again moves it.
func (s *Store) ClaimInstallation(ctx context.Context, id, teamID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE github_installations SET team_id = $2, updated_at = now() WHERE id = $1`, id, teamID)
	return affected(res, err)
}

// GetInstallation returns one installation.
func (s *Store) GetInstallation(ctx context.Context, id int64) (Installation, error) {
	var in Installation
	var team sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, account_login, account_type, team_id, suspended
		FROM github_installations WHERE id = $1`, id).
		Scan(&in.ID, &in.AccountLogin, &in.AccountType, &team, &in.Suspended)
	if errors.Is(err, sql.ErrNoRows) {
		return in, ErrNotFound
	}
	if team.Valid {
		in.TeamID = &team.Int64
	}
	return in, err
}

// SetInstallationRepos replaces the repositories of an installation (a full
// resync, or the "all repositories" list at install time).
func (s *Store) SetInstallationRepos(ctx context.Context, id int64, repos []InstallationRepo) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM github_installation_repos WHERE installation_id = $1`, id); err != nil {
			return err
		}
		return insertInstallationRepos(ctx, tx, id, repos)
	})
}

// AddInstallationRepos and RemoveInstallationRepos apply an
// installation_repositories event.
func (s *Store) AddInstallationRepos(ctx context.Context, id int64, repos []InstallationRepo) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return insertInstallationRepos(ctx, tx, id, repos) })
}

func (s *Store) RemoveInstallationRepos(ctx context.Context, id int64, repoIDs []int64) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM github_installation_repos WHERE installation_id = $1 AND repo_id = ANY($2)`,
		id, pq.Array(repoIDs))
	return err
}

func insertInstallationRepos(ctx context.Context, tx *sql.Tx, id int64, repos []InstallationRepo) error {
	for _, r := range repos {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO github_installation_repos (installation_id, repo_id, full_name, private)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (installation_id, repo_id) DO UPDATE
			SET full_name = EXCLUDED.full_name, private = EXCLUDED.private`,
			id, r.RepoID, r.FullName, r.Private); err != nil {
			return err
		}
	}
	return nil
}

// InstallationForRepo returns the installation that grants access to repo
// ("owner/name", case-insensitive). Suspended installations do not count.
// ErrNotFound means the App is not installed on it.
func (s *Store) InstallationForRepo(ctx context.Context, repo string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT r.installation_id FROM github_installation_repos r
		JOIN github_installations i ON i.id = r.installation_id
		WHERE lower(r.full_name) = lower($1) AND NOT i.suspended
		ORDER BY r.installation_id LIMIT 1`, repo).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// ImportableRepos lists the repositories of the installations claimed by the
// given teams, with the app already deployed from each (if any).
func (s *Store) ImportableRepos(ctx context.Context, teamIDs []int64) ([]ImportableRepo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.installation_id, r.repo_id, r.full_name, r.private, i.account_login,
			COALESCE((SELECT a.name FROM apps a WHERE lower(a.repo_full_name) = lower(r.full_name)
			          ORDER BY a.id LIMIT 1), '')
		FROM github_installation_repos r
		JOIN github_installations i ON i.id = r.installation_id
		WHERE i.team_id = ANY($1) AND NOT i.suspended
		ORDER BY lower(r.full_name)`, pq.Array(teamIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ImportableRepo{}
	for rows.Next() {
		var r ImportableRepo
		if err := rows.Scan(&r.InstallationID, &r.RepoID, &r.FullName, &r.Private, &r.AccountLogin, &r.App); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListInstallations returns all installations (admin view, resync).
func (s *Store) ListInstallations(ctx context.Context) ([]Installation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_login, account_type, team_id, suspended FROM github_installations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Installation{}
	for rows.Next() {
		var in Installation
		var team sql.NullInt64
		if err := rows.Scan(&in.ID, &in.AccountLogin, &in.AccountType, &team, &in.Suspended); err != nil {
			return nil, err
		}
		if team.Valid {
			in.TeamID = &team.Int64
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// tx runs fn in a transaction, committing when it returns nil.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// affected turns "no row updated" into ErrNotFound.
func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
