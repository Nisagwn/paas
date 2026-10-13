package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Faz 28: deleting a project (migration 021). The steps and why each one is
// safe to repeat are described in the migration and in internal/appdelete.

// AuditAppDelete is the audit action of a project deletion.
const AuditAppDelete = "app_delete"

// DeletedReason is the error recorded on deployments stopped by a deletion.
const DeletedReason = "project deleted"

var (
	// ErrAppDeleting: the name belongs to a project whose deletion has not
	// finished yet (CreateAppInTeam). It is a conflict.
	ErrAppDeleting = fmt.Errorf("%w: a project with this name is still being deleted", ErrConflict)
	// ErrDeploymentsRunning: DeleteAppRows waits for running deployments
	// to stop first.
	ErrDeploymentsRunning = errors.New("deployments of the app are still running")
)

// AppDeletion is the record of one project deletion.
type AppDeletion struct {
	ID            int64      `json:"id"`
	AppID         int64      `json:"app_id"`
	AppName       string     `json:"app_name"`
	Repo          string     `json:"repo"`
	TeamID        *int64     `json:"team_id,omitempty"`
	RequestedBy   string     `json:"requested_by"`
	RequestedAt   time.Time  `json:"requested_at"`
	RowsDeletedAt *time.Time `json:"rows_deleted_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"last_error,omitempty"`
}

const appDeletionCols = `id, app_id, app_name, repo_full_name, team_id, requested_by, requested_at,
	rows_deleted_at, finished_at, attempts, last_error`

func scanAppDeletion(row interface{ Scan(...any) error }) (AppDeletion, error) {
	var d AppDeletion
	var team sql.NullInt64
	err := row.Scan(&d.ID, &d.AppID, &d.AppName, &d.Repo, &team, &d.RequestedBy, &d.RequestedAt,
		&d.RowsDeletedAt, &d.FinishedAt, &d.Attempts, &d.LastError)
	if team.Valid {
		d.TeamID = &team.Int64
	}
	return d, err
}

// DeletionActor is who asked for a deletion: a login ("admin" for the
// admin principal) and the user id (nil for the admin principal).
type DeletionActor struct {
	Login  string
	UserID *int64
}

// BeginAppDeletion is step 1 of a deletion, in one transaction: it records
// the deletion (and an audit entry), hides the app (deleting_at), cancels
// its queued deployments and asks the running ones to stop. Calling it
// again for an app that is already being deleted returns the existing
// record. A missing app is ErrNotFound.
func (s *Store) BeginAppDeletion(ctx context.Context, appID int64, by DeletionActor) (AppDeletion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AppDeletion{}, err
	}
	defer tx.Rollback()
	var app App
	var deleting *time.Time
	err = tx.QueryRowContext(ctx, `SELECT `+appCols+`, deleting_at FROM apps WHERE id = $1 FOR UPDATE`, appID).
		Scan(&app.ID, &app.Name, &app.Repo, &app.ProductionBranch, &app.CreatedAt, &app.TeamID, &deleting)
	if errors.Is(err, sql.ErrNoRows) {
		return AppDeletion{}, ErrNotFound
	}
	if err != nil {
		return AppDeletion{}, err
	}
	if deleting != nil {
		return scanAppDeletion(tx.QueryRowContext(ctx,
			`SELECT `+appDeletionCols+` FROM app_deletions WHERE app_id = $1`, appID))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE apps SET deleting_at = now() WHERE id = $1`, appID); err != nil {
		return AppDeletion{}, err
	}
	if err := stopDeploymentsTx(ctx, tx, appID); err != nil {
		return AppDeletion{}, err
	}
	var uid any
	if by.UserID != nil && *by.UserID > 0 {
		uid = *by.UserID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (action, app_id, app_name, user_id, actor, ended_at)
		VALUES ($1, $2, $3, $4, $5, now())`, AuditAppDelete, appID, app.Name, uid, by.Login); err != nil {
		return AppDeletion{}, err
	}
	d, err := scanAppDeletion(tx.QueryRowContext(ctx, `
		INSERT INTO app_deletions (app_id, app_name, repo_full_name, team_id, requested_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+appDeletionCols,
		appID, app.Name, app.Repo, app.TeamID, by.Login))
	if err != nil {
		return AppDeletion{}, err
	}
	return d, tx.Commit()
}

// stopDeploymentsTx writes what CancelDeployment writes, for every
// unfinished deployment of the app: queued ones end at once, running ones
// get cancel_requested_at and their worker stops at the next heartbeat.
func stopDeploymentsTx(ctx context.Context, tx *sql.Tx, appID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE deployments SET
			status = CASE WHEN status = 'queued' THEN 'canceled' ELSE status END,
			error = CASE WHEN status = 'queued' THEN $2 ELSE error END,
			finished_at = CASE WHEN status = 'queued' THEN now() ELSE finished_at END,
			cleaned_at = CASE WHEN status = 'queued' THEN now() ELSE cleaned_at END,
			cancel_requested_at = COALESCE(cancel_requested_at, now())
		WHERE app_id = $1 AND status IN ('queued', 'building', 'deploying')`, appID, DeletedReason)
	return err
}

// GetAppDeletion returns one deletion record.
func (s *Store) GetAppDeletion(ctx context.Context, id int64) (AppDeletion, error) {
	d, err := scanAppDeletion(s.db.QueryRowContext(ctx, `SELECT `+appDeletionCols+` FROM app_deletions WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// PendingAppDeletions lists the deletions that have not finished, oldest first.
func (s *Store) PendingAppDeletions(ctx context.Context) ([]AppDeletion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appDeletionCols+` FROM app_deletions
		WHERE finished_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AppDeletion{}
	for rows.Next() {
		d, err := scanAppDeletion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ClaimAppDeletion takes the lease of an unfinished deletion for lease; it
// reports false when another runner holds it or the deletion finished.
func (s *Store) ClaimAppDeletion(ctx context.Context, id int64, lease time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE app_deletions SET claimed_until = now() + make_interval(secs => $2::float8)
		WHERE id = $1 AND finished_at IS NULL AND (claimed_until IS NULL OR claimed_until < now())`,
		id, lease.Seconds())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseAppDeletion gives the lease back, recording errMsg ("" when the
// step only has to wait) for the next attempt.
func (s *Store) ReleaseAppDeletion(ctx context.Context, id int64, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE app_deletions SET claimed_until = NULL,
			attempts = attempts + CASE WHEN $2 = '' THEN 0 ELSE 1 END,
			last_error = $2
		WHERE id = $1`, id, errMsg)
	return err
}

// DeleteAppRows is step 2 of a deletion, in one transaction: once no
// deployment of the app is building or deploying, it deletes the app row;
// every table that references it cascades (deployments and their logs,
// aliases, variables, domains, hooks, settings, rollouts, metrics, add-ons
// with their copies and backups, comments, protection, notification
// filters). Audit entries and notification events keep their rows without
// the reference. Notification channels that watched only this app are
// disabled, since without app rows a channel receives every app's events.
//
// It returns ErrDeploymentsRunning while a deployment still runs, and nil
// when the rows are already gone (a repeated step).
func (s *Store) DeleteAppRows(ctx context.Context, d AppDeletion) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var found bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id = $1 FOR UPDATE`, d.AppID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `UPDATE app_deletions SET rows_deleted_at = COALESCE(rows_deleted_at, now())
			WHERE id = $1`, d.ID); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	// A push that read the app before step 1 may have queued one since.
	if err := stopDeploymentsTx(ctx, tx, d.AppID); err != nil {
		return err
	}
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM deployments
		WHERE app_id = $1 AND status IN ('building', 'deploying')`, d.AppID).Scan(&running); err != nil {
		return err
	}
	if running > 0 {
		return ErrDeploymentsRunning
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE notification_channels c SET enabled = false, updated_at = now()
		WHERE c.enabled
		  AND EXISTS (SELECT 1 FROM notification_channel_apps ca WHERE ca.channel_id = c.id AND ca.app_id = $1)
		  AND NOT EXISTS (SELECT 1 FROM notification_channel_apps ca WHERE ca.channel_id = c.id AND ca.app_id <> $1)`,
		d.AppID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM apps WHERE id = $1`, d.AppID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE app_deletions SET rows_deleted_at = now() WHERE id = $1`, d.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishAppDeletion marks a deletion done (step 3 cleaned the cluster);
// the name can be used again.
func (s *Store) FinishAppDeletion(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE app_deletions SET finished_at = COALESCE(finished_at, now()),
		claimed_until = NULL, last_error = '' WHERE id = $1`, id)
	return err
}
