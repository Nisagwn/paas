// Package store persists apps, deployments, aliases and logs in PostgreSQL.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/nisagwn/paas/internal/secret"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
	ErrNotReady = errors.New("deployment is not ready")
)

// Deployment statuses. They mirror the CHECK constraint in the schema.
const (
	StatusQueued    = "queued"
	StatusBuilding  = "building"
	StatusDeploying = "deploying"
	StatusReady     = "ready"
	StatusFailed    = "failed"
)

const (
	AliasProduction = "production"
	AliasPreview    = "preview"
)

type App struct {
	ID               int64     `json:"id"`
	Name             string    `json:"name"`
	Repo             string    `json:"repo"`
	ProductionBranch string    `json:"production_branch"`
	CreatedAt        time.Time `json:"created_at"`
}

type Deployment struct {
	ID            int64      `json:"id"`
	AppID         int64      `json:"app_id"`
	AppName       string     `json:"app_name"`
	Repo          string     `json:"repo"`
	CommitSHA     string     `json:"commit_sha"`
	Branch        string     `json:"branch"`
	CommitMessage string     `json:"commit_message"`
	Status        string     `json:"status"`
	Error         string     `json:"error,omitempty"`
	Image         string     `json:"image,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	// Faz 7: retirement and crash recovery (lifecycle.go).
	RetiredAt    *time.Time `json:"retired_at,omitempty"`
	RetireReason string     `json:"retire_reason,omitempty"`
	Attempts     int        `json:"attempts"`
}

type Alias struct {
	Hostname     string    `json:"hostname"`
	Kind         string    `json:"kind"`
	Branch       string    `json:"branch"`
	DeploymentID int64     `json:"deployment_id"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type LogLine struct {
	ID   int64     `json:"id"`
	TS   time.Time `json:"ts"`
	Line string    `json:"line"`
}

type Store struct {
	db *sql.DB
	// Faz 10: encrypts app_env values; nil stores plaintext (envcrypt.go).
	env *secret.Keyring
}

func Open(ctx context.Context, url string) (*Store, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies every migrations/NNN_*.sql file that has not run yet.
// A Postgres advisory lock keeps concurrent instances from racing.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	const lockID = 727274 // arbitrary, unique to paas
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID)

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		version := strings.TrimSuffix(strings.TrimPrefix(f, "migrations/"), ".sql")
		var exists bool
		if err := conn.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		body, err := migrationFS.ReadFile(f)
		if err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---- apps ----

func (s *Store) CreateApp(ctx context.Context, name, repo, branch string) (App, error) {
	var a App
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO apps (name, repo_full_name, production_branch) VALUES ($1, $2, $3)
		RETURNING id, name, repo_full_name, production_branch, created_at`,
		name, repo, branch).Scan(&a.ID, &a.Name, &a.Repo, &a.ProductionBranch, &a.CreatedAt)
	if isUniqueViolation(err) {
		return a, ErrConflict
	}
	return a, err
}

func (s *Store) ListApps(ctx context.Context) ([]App, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, repo_full_name, production_branch, created_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	apps := []App{}
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.Repo, &a.ProductionBranch, &a.CreatedAt); err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

func (s *Store) GetAppByName(ctx context.Context, name string) (App, error) {
	return s.getApp(ctx, `name = $1`, name)
}

// GetAppByRepo matches GitHub's "owner/repo" case-insensitively.
func (s *Store) GetAppByRepo(ctx context.Context, repo string) (App, error) {
	return s.getApp(ctx, `lower(repo_full_name) = lower($1)`, repo)
}

func (s *Store) getApp(ctx context.Context, where string, arg any) (App, error) {
	var a App
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, repo_full_name, production_branch, created_at FROM apps WHERE `+where, arg).
		Scan(&a.ID, &a.Name, &a.Repo, &a.ProductionBranch, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// ---- deployments ----

const deploymentCols = `d.id, d.app_id, a.name, a.repo_full_name, d.commit_sha, d.branch, d.commit_message, d.status,
	d.error, d.image, d.created_at, d.started_at, d.finished_at, d.retired_at, d.retire_reason, d.attempts`

func scanDeployment(row interface{ Scan(...any) error }) (Deployment, error) {
	var d Deployment
	err := row.Scan(&d.ID, &d.AppID, &d.AppName, &d.Repo, &d.CommitSHA, &d.Branch, &d.CommitMessage, &d.Status,
		&d.Error, &d.Image, &d.CreatedAt, &d.StartedAt, &d.FinishedAt, &d.RetiredAt, &d.RetireReason, &d.Attempts)
	return d, err
}

// EnqueueDeployment queues a commit for deployment. Deployments are immutable,
// so pushing the same commit again returns the existing one with created=false.
// A retired deployment whose objects are gone is queued again (created=true).
func (s *Store) EnqueueDeployment(ctx context.Context, appID int64, sha, branch, message string) (Deployment, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO deployments (app_id, commit_sha, branch, commit_message) VALUES ($1, $2, $3, $4)
		ON CONFLICT (app_id, commit_sha) DO NOTHING
		RETURNING id`, appID, sha, branch, message).Scan(&id)
	created := true
	if errors.Is(err, sql.ErrNoRows) {
		created = false
		err = s.db.QueryRowContext(ctx,
			`SELECT id FROM deployments WHERE app_id = $1 AND commit_sha = $2`, appID, sha).Scan(&id)
		if err == nil {
			created, err = s.revive(ctx, id, branch, message)
		}
	}
	if err != nil {
		return Deployment{}, false, err
	}
	d, err := s.GetDeployment(ctx, id)
	return d, created, err
}

func (s *Store) GetDeployment(ctx context.Context, id int64) (Deployment, error) {
	d, err := scanDeployment(s.db.QueryRowContext(ctx, `
		SELECT `+deploymentCols+` FROM deployments d JOIN apps a ON a.id = d.app_id WHERE d.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

func (s *Store) ListDeployments(ctx context.Context, appID int64, limit int) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+deploymentCols+` FROM deployments d JOIN apps a ON a.id = d.app_id
		WHERE d.app_id = $1 ORDER BY d.id DESC LIMIT $2`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ClaimNext atomically moves the oldest queued deployment to "building" and
// returns it. FOR UPDATE SKIP LOCKED lets many workers poll without picking
// the same job. It returns ErrNotFound when the queue is empty.
func (s *Store) ClaimNext(ctx context.Context) (Deployment, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		UPDATE deployments SET status = 'building', started_at = now(),
			attempts = attempts + 1, heartbeat_at = now()
		WHERE id = (
			SELECT id FROM deployments WHERE status = 'queued'
			ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1
		)
		RETURNING id`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	return s.GetDeployment(ctx, id)
}

func (s *Store) SetStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET status = $2 WHERE id = $1`, id, status)
	return err
}

func (s *Store) SetImage(ctx context.Context, id int64, image string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET image = $2 WHERE id = $1`, id, image)
	return err
}

func (s *Store) MarkFailed(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE deployments SET status = 'failed', error = $2, finished_at = now() WHERE id = $1`, id, reason)
	return err
}

// AliasSpec describes an alias that should point at a deployment.
type AliasSpec struct {
	Hostname string
	Kind     string
	Branch   string
}

// MarkReady marks a deployment ready and points the given aliases at it, in
// one transaction. An alias only moves forward: a slow build of an older
// commit that finishes late will not overwrite a newer deployment.
//
// If the branch was deleted while the deployment was in flight (retired_at
// is set), it ends as retired without aliases and ErrRetired is returned.
func (s *Store) MarkReady(ctx context.Context, d Deployment, aliases []AliasSpec) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `
		UPDATE deployments SET status = CASE WHEN retired_at IS NULL THEN 'ready' ELSE 'retired' END,
			error = '', finished_at = now()
		WHERE id = $1 RETURNING status`, d.ID).Scan(&status); err != nil {
		return err
	}
	if status == StatusRetired {
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrRetired
	}
	for _, a := range aliases {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO aliases (app_id, hostname, kind, branch, deployment_id) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (hostname) DO UPDATE
				SET deployment_id = EXCLUDED.deployment_id, updated_at = now()
				WHERE aliases.deployment_id < EXCLUDED.deployment_id`,
			d.AppID, a.Hostname, a.Kind, a.Branch, d.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Rollback points the app's production alias at an earlier ready deployment.
// No rebuild happens: the old deployment is still running. Retired
// deployments are gone from the cluster and return ErrRetired.
func (s *Store) Rollback(ctx context.Context, appID, deploymentID int64) (Alias, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Alias{}, err
	}
	defer tx.Rollback()

	// FOR SHARE blocks a concurrent Retire of this row until the alias has
	// moved, and Retire then sees the alias and keeps the deployment.
	var status string
	err = tx.QueryRowContext(ctx, `
		SELECT status FROM deployments WHERE id = $1 AND app_id = $2 FOR SHARE`, deploymentID, appID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return Alias{}, ErrNotFound
	}
	if err != nil {
		return Alias{}, err
	}
	if status == StatusRetired {
		return Alias{}, ErrRetired
	}
	if status != StatusReady {
		return Alias{}, ErrNotReady
	}
	var a Alias
	err = tx.QueryRowContext(ctx, `
		UPDATE aliases SET deployment_id = $3, updated_at = now()
		WHERE app_id = $1 AND kind = $2
		RETURNING hostname, kind, branch, deployment_id, updated_at`,
		appID, AliasProduction, deploymentID).Scan(&a.Hostname, &a.Kind, &a.Branch, &a.DeploymentID, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// No production deployment has ever succeeded, so there is nothing to roll back.
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func (s *Store) ListAliases(ctx context.Context, appID int64) ([]Alias, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT hostname, kind, branch, deployment_id, updated_at FROM aliases
		WHERE app_id = $1 ORDER BY kind DESC, hostname`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alias{}
	for rows.Next() {
		var a Alias
		if err := rows.Scan(&a.Hostname, &a.Kind, &a.Branch, &a.DeploymentID, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- environment variables ----

// AppEnv returns all environment variables of an app, decrypted.
func (s *Store) AppEnv(ctx context.Context, appID int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value, key_id FROM app_env WHERE app_id = $1`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	env := map[string]string{}
	for rows.Next() {
		var k, v string
		var keyID sql.NullString
		if err := rows.Scan(&k, &v, &keyID); err != nil {
			return nil, err
		}
		if env[k], err = s.openEnv(appID, k, v, keyID); err != nil {
			return nil, err
		}
	}
	return env, rows.Err()
}

// UpdateAppEnv sets the given variables in one transaction; a nil value
// deletes the variable. Variables not mentioned are left alone.
func (s *Store) UpdateAppEnv(ctx context.Context, appID int64, changes map[string]*string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range changes {
		if v == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM app_env WHERE app_id = $1 AND key = $2`, appID, k)
		} else {
			value, keyID, serr := s.sealEnv(appID, k, *v)
			if serr != nil {
				return serr
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO app_env (app_id, key, value, key_id) VALUES ($1, $2, $3, $4)
				ON CONFLICT (app_id, key) DO UPDATE
				SET value = EXCLUDED.value, key_id = EXCLUDED.key_id, updated_at = now()`,
				appID, k, value, keyID)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AliasRoute is an alias joined with the commit it points at: everything
// the router needs to send a hostname to the right deployment.
type AliasRoute struct {
	Hostname     string
	Kind         string
	Branch       string
	DeploymentID int64
	CommitSHA    string
}

// AliasRoutes returns the app's aliases with their target commits.
func (s *Store) AliasRoutes(ctx context.Context, appID int64) ([]AliasRoute, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT al.hostname, al.kind, al.branch, al.deployment_id, d.commit_sha
		FROM aliases al JOIN deployments d ON d.id = al.deployment_id
		WHERE al.app_id = $1 ORDER BY al.hostname`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AliasRoute{}
	for rows.Next() {
		var r AliasRoute
		if err := rows.Scan(&r.Hostname, &r.Kind, &r.Branch, &r.DeploymentID, &r.CommitSHA); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- logs ----

func (s *Store) AppendLog(ctx context.Context, deploymentID int64, line string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO deployment_logs (deployment_id, line) VALUES ($1, $2)`, deploymentID, line)
	return err
}

// Logs returns log lines with id > afterID, so clients can poll incrementally.
func (s *Store) Logs(ctx context.Context, deploymentID, afterID int64, limit int) ([]LogLine, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, line FROM deployment_logs
		WHERE deployment_id = $1 AND id > $2 ORDER BY id LIMIT $3`, deploymentID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogLine{}
	for rows.Next() {
		var l LogLine
		if err := rows.Scan(&l.ID, &l.TS, &l.Line); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}
