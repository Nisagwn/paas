package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/nisagwn/paas/internal/naming"
)

// Faz 17: environments and deploy controls (migration 011).
//
// Every deployment runs in one of two environments, Vercel style:
// production (the production branch, and promoted deployments) or preview
// (every other branch). Variables are scoped to an environment; a preview
// variable may be narrowed to one branch.
//
// Redeploy and promote never mutate a deployment: they create a new one of
// the same commit with the next generation, its own env Secret and its own
// objects (naming.ObjectName). When the source has an image, the new one
// reuses it and the worker skips the build.
//
// State machine with cancellation:
//
//	queued ──cancel──► canceled (at once; nothing was created)
//	building/deploying ──cancel──► cancel_requested_at set → the worker's
//	    heartbeat loop cancels the run → canceled (objects cleaned up)

// Environment targets of variables and deployments.
const (
	EnvAll        = "all" // variables only: both environments
	EnvProduction = "production"
	EnvPreview    = "preview"
)

// Deployment origins.
const (
	OriginGit      = "git"
	OriginRedeploy = "redeploy"
	OriginPromote  = "promote"
	OriginHook     = "hook"
)

// StatusCanceled: stopped by a user before it finished.
const StatusCanceled = "canceled"

// CanceledReason is the error recorded on a canceled deployment.
const CanceledReason = "canceled by a user"

var (
	// ErrCanceled is returned by MarkReady when the deployment's
	// cancellation was requested while it ran.
	ErrCanceled = errors.New("deployment was canceled")
	// ErrFinished: the deployment already reached a final status.
	ErrFinished = errors.New("deployment already finished")
	// ErrInFlight: the deployment is still queued, building or deploying.
	ErrInFlight = errors.New("deployment has not finished yet")
	// ErrInvalid: a request the store refuses on its own (bad target etc.).
	ErrInvalid = errors.New("invalid request")
)

// ObjectName is the Kubernetes name of the deployment's objects.
func (d Deployment) ObjectName() string { return naming.ObjectName(d.CommitSHA, d.Generation) }

// Host is the deployment's immutable hostname under domain.
func (d Deployment) Host(domain string) string {
	return naming.InstanceHost(d.CommitSHA, d.Generation, d.AppName, domain)
}

// ---- environment variables ----

// EnvVar is one stored variable row; values never leave the store through
// it (ListEnv leaves Value empty).
type EnvVar struct {
	Key       string    `json:"key"`
	Target    string    `json:"target"`
	GitBranch string    `json:"git_branch,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ValidEnvTarget reports whether t is a target a variable can have.
func ValidEnvTarget(t string) bool {
	return t == EnvAll || t == EnvProduction || t == EnvPreview
}

type envRow struct {
	key, value string
	sc         envScope
	keyID      sql.NullString
	updatedAt  time.Time
}

func (s *Store) envRows(ctx context.Context, appID int64) ([]envRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key, target, git_branch, value, key_id, updated_at FROM app_env
		WHERE app_id = $1 ORDER BY key, target, git_branch`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []envRow
	for rows.Next() {
		var r envRow
		if err := rows.Scan(&r.key, &r.sc.target, &r.sc.branch, &r.value, &r.keyID, &r.updatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListEnv returns the app's variable rows without values.
func (s *Store) ListEnv(ctx context.Context, appID int64) ([]EnvVar, error) {
	rows, err := s.envRows(ctx, appID)
	if err != nil {
		return nil, err
	}
	out := make([]EnvVar, 0, len(rows))
	for _, r := range rows {
		out = append(out, EnvVar{Key: r.key, Target: r.sc.target, GitBranch: r.sc.branch, UpdatedAt: r.updatedAt})
	}
	return out, nil
}

// envRank orders rows from least to most specific for target/branch; -1
// means the row does not apply.
func envRank(sc envScope, target, branch string) int {
	switch {
	case sc.target == EnvAll && sc.branch == "":
		return 0
	case sc.target == target && sc.branch == "":
		return 1
	case target == EnvPreview && sc.target == EnvPreview && sc.branch == branch:
		return 2
	}
	return -1
}

// ResolveEnv returns the decrypted variables a deployment in target on
// branch runs with: production = all < production; preview = all < preview
// < preview of the branch (most specific wins).
func (s *Store) ResolveEnv(ctx context.Context, appID int64, target, branch string) (map[string]string, error) {
	if target != EnvProduction && target != EnvPreview {
		return nil, ErrInvalid
	}
	rows, err := s.envRows(ctx, appID)
	if err != nil {
		return nil, err
	}
	env, rank := map[string]string{}, map[string]int{}
	for _, r := range rows {
		n := envRank(r.sc, target, branch)
		if n < 0 {
			continue
		}
		if have, ok := rank[r.key]; ok && have > n {
			continue
		}
		v, err := s.openEnv(appID, r.key, r.sc, r.value, r.keyID)
		if err != nil {
			return nil, err
		}
		env[r.key], rank[r.key] = v, n
	}
	return env, nil
}

// DeploymentEnv is ResolveEnv for d's environment and branch.
func (s *Store) DeploymentEnv(ctx context.Context, d Deployment) (map[string]string, error) {
	target := d.Target
	if target == "" {
		target = EnvPreview
	}
	return s.ResolveEnv(ctx, d.AppID, target, d.Branch)
}

// AppEnv returns every variable name of the app with one value each, all
// rows decrypted: the production value when there is one, else the
// preview value, else a branch override. It is the flat view of the
// callers that predate environments (web UI key list, tests).
func (s *Store) AppEnv(ctx context.Context, appID int64) (map[string]string, error) {
	rows, err := s.envRows(ctx, appID)
	if err != nil {
		return nil, err
	}
	rank := func(sc envScope) int {
		switch {
		case sc.target == EnvProduction:
			return 3
		case sc.target == EnvAll:
			return 2
		case sc.branch == "":
			return 1
		}
		return 0
	}
	env, best := map[string]string{}, map[string]int{}
	for _, r := range rows {
		v, err := s.openEnv(appID, r.key, r.sc, r.value, r.keyID)
		if err != nil {
			return nil, err
		}
		if have, ok := best[r.key]; ok && have >= rank(r.sc) {
			continue
		}
		env[r.key], best[r.key] = v, rank(r.sc)
	}
	return env, nil
}

// EnvChange sets (Value non-nil) or deletes (nil) one variable in one scope.
//
// Target "" or "all" is the scope of API calls without a target, "both
// environments": setting writes the "all" row and removes the variable's
// production and preview rows without a branch, so both environments see
// the value (branch overrides stay: they are explicit exceptions);
// deleting removes the variable everywhere, branch overrides included.
type EnvChange struct {
	Key       string
	Value     *string
	Target    string
	GitBranch string
}

// UpdateAppEnv sets the given variables for both environments in one
// transaction; a nil value deletes the variable. Variables not mentioned
// are left alone.
func (s *Store) UpdateAppEnv(ctx context.Context, appID int64, changes map[string]*string) error {
	list := make([]EnvChange, 0, len(changes))
	for k, v := range changes {
		list = append(list, EnvChange{Key: k, Value: v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	return s.ApplyEnvChanges(ctx, appID, list)
}

// ApplyEnvChanges applies changes in one transaction.
func (s *Store) ApplyEnvChanges(ctx context.Context, appID int64, changes []EnvChange) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range changes {
		sc := envScope{target: c.Target, branch: c.GitBranch}
		if sc.target == "" {
			sc.target = EnvAll
		}
		if !ValidEnvTarget(sc.target) || (sc.branch != "" && sc.target != EnvPreview) {
			return ErrInvalid
		}
		all := sc.target == EnvAll
		switch {
		case c.Value == nil && all:
			_, err = tx.ExecContext(ctx, `DELETE FROM app_env WHERE app_id = $1 AND key = $2`, appID, c.Key)
		case c.Value == nil:
			_, err = tx.ExecContext(ctx, `
				DELETE FROM app_env WHERE app_id = $1 AND key = $2 AND target = $3 AND git_branch = $4`,
				appID, c.Key, sc.target, sc.branch)
		default:
			if all {
				if _, err := tx.ExecContext(ctx, `
					DELETE FROM app_env WHERE app_id = $1 AND key = $2
					  AND target IN ('production', 'preview') AND git_branch = ''`, appID, c.Key); err != nil {
					return err
				}
			}
			value, keyID, serr := s.sealEnv(appID, c.Key, sc, *c.Value)
			if serr != nil {
				return serr
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO app_env (app_id, key, target, git_branch, value, key_id) VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (app_id, key, target, git_branch) DO UPDATE
				SET value = EXCLUDED.value, key_id = EXCLUDED.key_id, updated_at = now()`,
				appID, c.Key, sc.target, sc.branch, value, keyID)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- redeploy and promote ----

// CopyOptions describes a deployment created from an existing one.
type CopyOptions struct {
	Origin string // OriginRedeploy, OriginPromote or OriginHook
	Target string // environment of the new deployment
	// ReuseImage copies the source's image: the worker deploys it without
	// building. Without an image on the source, it is built again.
	ReuseImage bool
	Message    string // "" keeps the source's commit message
}

// CopyDeployment queues a new deployment of src's commit and branch with the
// next generation. src must have finished (ErrInFlight otherwise): a copy
// of a running deployment could not reuse its image yet.
func (s *Store) CopyDeployment(ctx context.Context, src Deployment, o CopyOptions) (Deployment, error) {
	if o.Target != EnvProduction && o.Target != EnvPreview {
		return Deployment{}, ErrInvalid
	}
	if !src.Finished() {
		return Deployment{}, ErrInFlight
	}
	image := ""
	if o.ReuseImage {
		image = src.Image
	}
	message := o.Message
	if message == "" {
		message = src.CommitMessage
	}
	var id int64
	var err error
	// Two concurrent copies may pick the same generation; the unique
	// constraint rejects one and it retries with the next.
	for attempt := 0; attempt < 5; attempt++ {
		err = s.db.QueryRowContext(ctx, `
			INSERT INTO deployments (app_id, commit_sha, branch, commit_message, image, target, origin,
				source_deployment_id, generation)
			SELECT $1, $2, $3, $4, $5, $6, $7, $8,
				(SELECT COALESCE(max(generation), 0) + 1 FROM deployments WHERE app_id = $1 AND commit_sha = $2)
			RETURNING id`,
			src.AppID, src.CommitSHA, src.Branch, message, image, o.Target, o.Origin, src.ID).Scan(&id)
		if !isUniqueViolation(err) {
			break
		}
	}
	if err != nil {
		return Deployment{}, err
	}
	if image != "" {
		// Faz 20: the image's process set comes with it.
		if err := s.copyProcesses(ctx, src.ID, id); err != nil {
			return Deployment{}, err
		}
	}
	return s.GetDeployment(ctx, id)
}

// ---- cancellation ----

// CancelDeployment cancels a deployment. A queued one is canceled at once
// (nothing was created for it); for a building or deploying one the request
// is recorded and the worker running it stops at its next heartbeat. The
// returned deployment shows which happened. Finished deployments return
// ErrFinished.
func (s *Store) CancelDeployment(ctx context.Context, id int64) (Deployment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Deployment{}, err
	}
	defer tx.Rollback()
	// FOR UPDATE: ClaimNext skips a locked row, so a queued deployment is
	// either claimed before this or canceled here, never both.
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM deployments WHERE id = $1 FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	switch status {
	case StatusQueued:
		_, err = tx.ExecContext(ctx, `
			UPDATE deployments SET status = 'canceled', error = $2, cancel_requested_at = now(),
				finished_at = now(), cleaned_at = now()
			WHERE id = $1`, id, CanceledReason)
	case StatusBuilding, StatusDeploying:
		_, err = tx.ExecContext(ctx, `
			UPDATE deployments SET cancel_requested_at = COALESCE(cancel_requested_at, now()) WHERE id = $1`, id)
	default:
		return Deployment{}, ErrFinished
	}
	if err != nil {
		return Deployment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Deployment{}, err
	}
	return s.GetDeployment(ctx, id)
}

// HeartbeatState is what the worker learns from one heartbeat.
type HeartbeatState struct {
	// Owned: the attempt still owns the deployment.
	Owned bool
	// CancelRequested: a user asked to cancel the deployment.
	CancelRequested bool
}

// Beat records that the worker running attempt of deployment id is alive
// and reports whether it still owns it and whether cancellation was
// requested.
func (s *Store) Beat(ctx context.Context, id int64, attempt int) (HeartbeatState, error) {
	var st HeartbeatState
	err := s.db.QueryRowContext(ctx, `
		UPDATE deployments SET heartbeat_at = now()
		WHERE id = $1 AND attempts = $2 AND status <> 'queued'
		RETURNING cancel_requested_at IS NOT NULL`, id, attempt).Scan(&st.CancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	st.Owned = err == nil
	return st, err
}

// ---- deploy hooks ----

// DeployHook is a secret URL that deploys the head of Branch.
type DeployHook struct {
	ID              int64      `json:"id"`
	AppID           int64      `json:"app_id"`
	Name            string     `json:"name"`
	Branch          string     `json:"branch"`
	Prefix          string     `json:"prefix"`
	CreatedAt       time.Time  `json:"created_at"`
	LastTriggeredAt *time.Time `json:"last_triggered_at,omitempty"`
}

const hookCols = `id, app_id, name, branch, prefix, created_at, last_triggered_at`

func scanHook(row interface{ Scan(...any) error }) (DeployHook, error) {
	var h DeployHook
	err := row.Scan(&h.ID, &h.AppID, &h.Name, &h.Branch, &h.Prefix, &h.CreatedAt, &h.LastTriggeredAt)
	return h, err
}

// CreateDeployHook stores a hook; only the token's SHA-256 is kept.
// createdBy 0 means the admin token.
func (s *Store) CreateDeployHook(ctx context.Context, appID int64, name, branch, tokenHash, prefix string, createdBy int64) (DeployHook, error) {
	h, err := scanHook(s.db.QueryRowContext(ctx, `
		INSERT INTO deploy_hooks (app_id, name, branch, token_hash, prefix, created_by)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, 0))
		RETURNING `+hookCols, appID, name, branch, tokenHash, prefix, createdBy))
	if isUniqueViolation(err) {
		return h, ErrConflict
	}
	return h, err
}

// ListDeployHooks returns the app's hooks, oldest first.
func (s *Store) ListDeployHooks(ctx context.Context, appID int64) ([]DeployHook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+hookCols+` FROM deploy_hooks WHERE app_id = $1 ORDER BY id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeployHook{}
	for rows.Next() {
		h, err := scanHook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// DeleteDeployHook removes one of the app's hooks.
func (s *Store) DeleteDeployHook(ctx context.Context, appID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM deploy_hooks WHERE id = $1 AND app_id = $2`, id, appID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TriggerDeployHook finds the hook with tokenHash and records the trigger.
func (s *Store) TriggerDeployHook(ctx context.Context, tokenHash string) (DeployHook, error) {
	h, err := scanHook(s.db.QueryRowContext(ctx, `
		UPDATE deploy_hooks SET last_triggered_at = now() WHERE token_hash = $1
		RETURNING `+hookCols, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

// GetAppByID loads an app by id.
func (s *Store) GetAppByID(ctx context.Context, id int64) (App, error) {
	return s.getApp(ctx, `id = $1`, id)
}
