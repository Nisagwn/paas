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
	// Faz 13: the owning team (users.go).
	TeamID int64 `json:"team_id"`
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
	// Faz 11: set while the deployment is scaled to zero (scale.go).
	SleepingSince *time.Time `json:"sleeping_since,omitempty"`
	// Faz 16: the framework the build used, e.g. "Next.js" (build_settings.go).
	Framework string `json:"framework,omitempty"`
	// Faz 17 (environments.go). Target is the environment whose variables
	// the deployment runs with and whether it takes the production alias.
	Target string `json:"target"`
	// Origin: git (push, import), redeploy, promote or hook.
	Origin string `json:"origin"`
	// SourceDeploymentID: the deployment a redeploy or promotion copied.
	SourceDeploymentID *int64 `json:"source_deployment_id,omitempty"`
	// Generation: 0 for a commit's first deployment, n for its n-th redeploy
	// or promotion. It keeps object names and URLs unique per deployment.
	Generation        int        `json:"generation"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty"`
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
func (s *Store) Migrate(ctx context.Context) error { return s.migrate(ctx, "") }

// migrate stops after version until ("" applies everything); tests use it to
// check data migrations against rows written by an older schema.
func (s *Store) migrate(ctx context.Context, until string) error {
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
		if until != "" && version > until {
			break
		}
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

// appCols are the columns scanned by scanApp.
const appCols = `id, name, repo_full_name, production_branch, created_at, team_id`

func scanApp(row interface{ Scan(...any) error }) (App, error) {
	var a App
	err := row.Scan(&a.ID, &a.Name, &a.Repo, &a.ProductionBranch, &a.CreatedAt, &a.TeamID)
	return a, err
}

// CreateApp creates an app in the "default" team; CreateAppInTeam picks one.
func (s *Store) CreateApp(ctx context.Context, name, repo, branch string) (App, error) {
	return s.CreateAppInTeam(ctx, 0, name, repo, branch)
}

// CreateAppInTeam creates an app owned by teamID (0: the "default" team).
func (s *Store) CreateAppInTeam(ctx context.Context, teamID int64, name, repo, branch string) (App, error) {
	a, err := scanApp(s.db.QueryRowContext(ctx, `
		INSERT INTO apps (name, repo_full_name, production_branch, team_id)
		VALUES ($1, $2, $3, COALESCE(NULLIF($4, 0), (SELECT id FROM teams WHERE slug = 'default')))
		RETURNING `+appCols, name, repo, branch, teamID))
	if isUniqueViolation(err) {
		return a, ErrConflict
	}
	return a, err
}

func (s *Store) ListApps(ctx context.Context) ([]App, error) {
	return s.listApps(ctx, `SELECT `+appCols+` FROM apps ORDER BY name`)
}

func (s *Store) listApps(ctx context.Context, query string, args ...any) ([]App, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	apps := []App{}
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
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
	a, err := scanApp(s.db.QueryRowContext(ctx, `SELECT `+appCols+` FROM apps WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// ---- deployments ----

const deploymentCols = `d.id, d.app_id, a.name, a.repo_full_name, d.commit_sha, d.branch, d.commit_message, d.status,
	d.error, d.image, d.created_at, d.started_at, d.finished_at, d.retired_at, d.retire_reason, d.attempts,
	d.sleeping_since, d.framework, d.target, d.origin, d.source_deployment_id, d.generation, d.cancel_requested_at`

func scanDeployment(row interface{ Scan(...any) error }) (Deployment, error) {
	var d Deployment
	err := row.Scan(&d.ID, &d.AppID, &d.AppName, &d.Repo, &d.CommitSHA, &d.Branch, &d.CommitMessage, &d.Status,
		&d.Error, &d.Image, &d.CreatedAt, &d.StartedAt, &d.FinishedAt, &d.RetiredAt, &d.RetireReason, &d.Attempts,
		&d.SleepingSince, &d.Framework, &d.Target, &d.Origin, &d.SourceDeploymentID, &d.Generation, &d.CancelRequestedAt)
	return d, err
}

// EnqueueDeployment queues a commit for deployment. Deployments are immutable,
// so pushing the same commit again returns the existing one with created=false.
// A retired deployment whose objects are gone is queued again (created=true).
//
// Faz 17: the target environment follows the branch (production branch →
// production, others → preview); origin records what queued it.
func (s *Store) EnqueueDeployment(ctx context.Context, appID int64, sha, branch, message string) (Deployment, bool, error) {
	return s.EnqueueDeploymentFrom(ctx, appID, sha, branch, message, OriginGit)
}

// EnqueueDeploymentFrom is EnqueueDeployment with an explicit origin (git or hook).
func (s *Store) EnqueueDeploymentFrom(ctx context.Context, appID int64, sha, branch, message, origin string) (Deployment, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO deployments (app_id, commit_sha, branch, commit_message, origin, target)
		SELECT $1, $2, $3, $4, $5,
			CASE WHEN a.production_branch = $3 THEN 'production' ELSE 'preview' END
		FROM apps a WHERE a.id = $1
		ON CONFLICT (app_id, commit_sha, generation) DO NOTHING
		RETURNING id`, appID, sha, branch, message, origin).Scan(&id)
	created := true
	if errors.Is(err, sql.ErrNoRows) {
		created = false
		err = s.db.QueryRowContext(ctx,
			`SELECT id FROM deployments WHERE app_id = $1 AND commit_sha = $2 AND generation = 0`, appID, sha).Scan(&id)
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

// MarkFailed records a failed run. A deployment whose cancellation was
// requested ends as canceled instead: the failure is then most likely the
// cancellation itself.
func (s *Store) MarkFailed(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE deployments SET
			status = CASE WHEN cancel_requested_at IS NULL THEN 'failed' ELSE 'canceled' END,
			error = CASE WHEN cancel_requested_at IS NULL THEN $2 ELSE '`+CanceledReason+`' END,
			finished_at = now()
		WHERE id = $1`, id, reason)
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
// If its cancellation was requested meanwhile (Faz 17), it ends as canceled
// without aliases and ErrCanceled is returned.
func (s *Store) MarkReady(ctx context.Context, d Deployment, aliases []AliasSpec) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `
		UPDATE deployments SET status = CASE WHEN retired_at IS NOT NULL THEN 'retired'
				WHEN cancel_requested_at IS NOT NULL THEN 'canceled' ELSE 'ready' END,
			error = CASE WHEN retired_at IS NULL AND cancel_requested_at IS NOT NULL THEN '`+CanceledReason+`' ELSE '' END,
			finished_at = now()
		WHERE id = $1 RETURNING status`, d.ID).Scan(&status); err != nil {
		return err
	}
	if status == StatusRetired || status == StatusCanceled {
		if err := tx.Commit(); err != nil {
			return err
		}
		if status == StatusCanceled {
			return ErrCanceled
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

// AliasRoute is an alias joined with the commit it points at: everything
// the router needs to send a hostname to the right deployment.
type AliasRoute struct {
	Hostname     string
	Kind         string
	Branch       string
	DeploymentID int64
	CommitSHA    string
	// Generation of the target deployment (Faz 17): with CommitSHA it names
	// the Service (naming.ObjectName).
	Generation int
}

// AliasRoutes returns the app's aliases with their target commits, plus
// one route of kind AliasCustom per routed custom domain. Custom domains
// target the production alias's deployment, so a rollback moves them too.
func (s *Store) AliasRoutes(ctx context.Context, appID int64) ([]AliasRoute, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT al.hostname, al.kind, al.branch, al.deployment_id, d.commit_sha, d.generation
		FROM aliases al JOIN deployments d ON d.id = al.deployment_id
		WHERE al.app_id = $1
		UNION ALL
		SELECT dm.hostname, '`+AliasCustom+`', al.branch, al.deployment_id, d.commit_sha, d.generation
		FROM app_domains dm
		JOIN aliases al ON al.app_id = dm.app_id AND al.kind = '`+AliasProduction+`'
		JOIN deployments d ON d.id = al.deployment_id
		WHERE dm.app_id = $1 AND dm.routed
		ORDER BY 1`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AliasRoute{}
	for rows.Next() {
		var r AliasRoute
		if err := rows.Scan(&r.Hostname, &r.Kind, &r.Branch, &r.DeploymentID, &r.CommitSHA, &r.Generation); err != nil {
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
