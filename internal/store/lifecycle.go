package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Faz 7: deployments end their life as "retired" (Kubernetes objects deleted,
// row kept for history), and deployments whose worker died are recovered.
//
//	ready ──(GC policy, branch deleted, PR closed)──► retired
//	building/deploying ──(stale heartbeat)──► queued (retry) | failed
//
// Concurrency: Retire locks the deployment row FOR UPDATE and only then
// checks for aliases, while Rollback holds FOR SHARE on its target while it
// moves the alias. Whichever commits first wins and the other sees it: a
// deployment that receives traffic is never retired, and a retired one never
// receives traffic.

// StatusRetired: the deployment was removed from the cluster.
const StatusRetired = "retired"

// ErrRetired is returned for operations that need a running deployment.
var ErrRetired = errors.New("deployment is retired")

// revive queues a retired deployment again when its commit is pushed again
// (e.g. a revert to an old commit). Only once its objects are gone, so the
// cleanup can never delete objects of the new attempt.
func (s *Store) revive(ctx context.Context, id int64, branch, message string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE deployments d SET status = 'queued', branch = $2,
			target = CASE WHEN a.production_branch = $2 THEN 'production' ELSE 'preview' END,
			cancel_requested_at = NULL,
			commit_message = COALESCE(NULLIF($3, ''), d.commit_message),
			error = '', image = '', started_at = NULL, finished_at = NULL,
			retired_at = NULL, retire_reason = '', cleaned_at = NULL, attempts = 0, heartbeat_at = NULL
		FROM apps a
		WHERE d.id = $1 AND a.id = d.app_id AND d.status = 'retired' AND d.cleaned_at IS NOT NULL`, id, branch, message)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ---- crash recovery ----

// Heartbeat records that the worker running attempt of deployment id is
// alive. It reports false when the attempt lost its claim: the deployment was
// recovered (queued again) or claimed by another worker in the meantime.
func (s *Store) Heartbeat(ctx context.Context, id int64, attempt int) (bool, error) {
	st, err := s.Beat(ctx, id, attempt)
	return st.Owned, err
}

// Recovered describes a deployment taken back from a dead worker.
type Recovered struct {
	ID       int64
	AppName  string
	Status   string // queued (retried), failed (out of attempts), retired or canceled
	Attempts int
}

// RecoverStale takes back building/deploying deployments whose heartbeat is
// older than staleAfter: their worker died (crash, OOM kill, node loss). A
// deployment that has been claimed fewer than maxAttempts times is queued
// again (build and deploy are idempotent); otherwise it fails with reason.
// One whose branch was deleted meanwhile is retired.
//
// The heartbeat makes this safe with several control-plane replicas: a live
// worker keeps its deployments fresh, so only orphans are taken.
func (s *Store) RecoverStale(ctx context.Context, staleAfter time.Duration, maxAttempts int, reason string) ([]Recovered, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH stale AS (
			SELECT id FROM deployments
			WHERE status IN ('building', 'deploying')
			  AND COALESCE(heartbeat_at, started_at, created_at) < now() - make_interval(secs => $1::float8)
			ORDER BY id FOR UPDATE SKIP LOCKED
		)
		UPDATE deployments d SET
			status = CASE WHEN d.retired_at IS NOT NULL THEN 'retired'
			              WHEN d.cancel_requested_at IS NOT NULL THEN 'canceled'
			              WHEN d.attempts < $2 THEN 'queued' ELSE 'failed' END,
			error = CASE WHEN d.retired_at IS NULL AND d.cancel_requested_at IS NOT NULL THEN '`+CanceledReason+`'
			             WHEN d.retired_at IS NULL AND d.attempts >= $2 THEN $3 ELSE d.error END,
			started_at = CASE WHEN d.retired_at IS NULL AND d.cancel_requested_at IS NULL AND d.attempts < $2
			                  THEN NULL ELSE d.started_at END,
			finished_at = CASE WHEN d.retired_at IS NULL AND d.cancel_requested_at IS NULL AND d.attempts < $2
			                   THEN NULL ELSE now() END,
			heartbeat_at = NULL
		FROM stale, apps a
		WHERE d.id = stale.id AND a.id = d.app_id
		RETURNING d.id, a.name, d.status, d.attempts`,
		staleAfter.Seconds(), maxAttempts, reason)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recovered
	for rows.Next() {
		var r Recovered
		if err := rows.Scan(&r.ID, &r.AppName, &r.Status, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- retirement ----

// RetireCandidates returns the app's ready deployments that the retention
// policy no longer needs. A deployment any alias points at is never one.
// Of the rest:
//   - production environment (the production branch, and promotions of
//     Faz 17): all but the newest keepProduction ready ones (rollback
//     targets);
//   - preview environment: those finished more than previewTTL ago
//     (previewTTL <= 0 keeps them).
func (s *Store) RetireCandidates(ctx context.Context, appID int64, keepProduction int, previewTTL time.Duration) ([]Deployment, error) {
	if keepProduction < 0 {
		keepProduction = 0
	}
	return s.queryDeployments(ctx, `
		SELECT `+deploymentCols+` FROM deployments d JOIN apps a ON a.id = d.app_id
		WHERE d.app_id = $1 AND d.status = 'ready'
		  AND NOT EXISTS (SELECT 1 FROM aliases al WHERE al.deployment_id = d.id)
		  AND (
		    (d.target = 'production' AND d.id NOT IN (
		        SELECT p.id FROM deployments p
		        WHERE p.app_id = $1 AND p.status = 'ready' AND p.target = 'production'
		        ORDER BY p.id DESC LIMIT $2))
		    OR ($3::float8 > 0 AND d.target = 'preview'
		        AND d.finished_at < now() - make_interval(secs => $3::float8))
		  )
		ORDER BY d.id`, appID, keepProduction, previewTTL.Seconds())
}

// Retire marks a ready deployment retired unless an alias points at it.
// It reports whether it did; the caller then deletes the cluster objects
// (PendingCleanup / MarkCleaned).
func (s *Store) Retire(ctx context.Context, id int64, reason string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM deployments WHERE id = $1 FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil || status != StatusReady {
		return false, err
	}
	// A new statement sees aliases committed while we waited for the lock.
	var aliased bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM aliases WHERE deployment_id = $1)`, id).Scan(&aliased); err != nil || aliased {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE deployments SET status = 'retired', retired_at = now(), retire_reason = $2
		WHERE id = $1`, id, reason); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// PendingCleanup returns retired, failed and canceled deployments of the app whose
// cluster objects have not been confirmed deleted yet (oldest first).
func (s *Store) PendingCleanup(ctx context.Context, appID int64, limit int) ([]Deployment, error) {
	return s.queryDeployments(ctx, `
		SELECT `+deploymentCols+` FROM deployments d JOIN apps a ON a.id = d.app_id
		WHERE d.app_id = $1 AND d.status IN ('retired', 'failed', 'canceled') AND d.cleaned_at IS NULL
		ORDER BY d.id LIMIT $2`, appID, limit)
}

// MarkCleaned records that the deployment's cluster objects are gone.
func (s *Store) MarkCleaned(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE deployments SET cleaned_at = now()
		WHERE id = $1 AND status IN ('retired', 'failed', 'canceled') AND cleaned_at IS NULL`, id)
	return err
}

// BranchCleanup reports what DeleteBranch changed.
type BranchCleanup struct {
	AliasesRemoved int `json:"aliases_removed"`
	// Ready deployments retired. One still serving production (after a
	// rollback to a preview deployment) is kept.
	Retired int `json:"retired"`
	// Queued deployments dropped before they started.
	Cancelled int `json:"cancelled"`
	// Deployments being built or deployed; they end as retired.
	InFlight int `json:"in_flight"`
}

// DeleteBranch handles a deleted branch (or a closed pull request): its
// preview alias goes away and its preview deployments are retired, all in
// one transaction. The production branch is never passed here (callers
// check). Production deployments of the branch (Faz 17 promotions) are
// left to the production retention rule.
func (s *Store) DeleteBranch(ctx context.Context, appID int64, branch, reason string) (BranchCleanup, error) {
	var bc BranchCleanup
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return bc, err
	}
	defer tx.Rollback()

	count := func(n *int, query string, args ...any) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		m, err := res.RowsAffected()
		*n = int(m)
		return err
	}
	if err := count(&bc.AliasesRemoved, `
		DELETE FROM aliases WHERE app_id = $1 AND kind = 'preview' AND branch = $2`, appID, branch); err != nil {
		return bc, err
	}
	// Nothing was created for queued ones, so they need no cleanup.
	if err := count(&bc.Cancelled, `
		UPDATE deployments SET status = 'retired', retired_at = now(), retire_reason = $3,
			finished_at = now(), cleaned_at = now()
		WHERE app_id = $1 AND branch = $2 AND status = 'queued' AND target = 'preview'`, appID, branch, reason); err != nil {
		return bc, err
	}
	// MarkReady turns these into retired instead of ready.
	if err := count(&bc.InFlight, `
		UPDATE deployments SET retired_at = now(), retire_reason = $3
		WHERE app_id = $1 AND branch = $2 AND status IN ('building', 'deploying') AND retired_at IS NULL
		  AND target = 'preview'`,
		appID, branch, reason); err != nil {
		return bc, err
	}
	// Lock first, then check aliases in a new statement (see Retire).
	if _, err := tx.ExecContext(ctx, `
		SELECT id FROM deployments WHERE app_id = $1 AND branch = $2 AND status = 'ready' AND target = 'preview'
		ORDER BY id FOR UPDATE`,
		appID, branch); err != nil {
		return bc, err
	}
	if err := count(&bc.Retired, `
		UPDATE deployments d SET status = 'retired', retired_at = now(), retire_reason = $3
		WHERE d.app_id = $1 AND d.branch = $2 AND d.status = 'ready' AND d.target = 'preview'
		  AND NOT EXISTS (SELECT 1 FROM aliases al WHERE al.deployment_id = d.id)`,
		appID, branch, reason); err != nil {
		return bc, err
	}
	return bc, tx.Commit()
}

// ExpirePreviewAliases removes preview aliases of non-production branches
// that have not moved for longer than ttl ("N days after the last push").
// Their deployments then fall under the preview retention rule.
func (s *Store) ExpirePreviewAliases(ctx context.Context, appID int64, ttl time.Duration) (int, error) {
	if ttl <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM aliases al USING apps a
		WHERE al.app_id = $1 AND a.id = al.app_id AND al.kind = 'preview'
		  AND al.branch <> a.production_branch
		  AND al.updated_at < now() - make_interval(secs => $2::float8)`, appID, ttl.Seconds())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) queryDeployments(ctx context.Context, query string, args ...any) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
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
