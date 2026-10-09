package store

import (
	"context"
	"database/sql"
	"time"
)

// Faz 27: the audit log (migration 020). The browser terminal records one
// row per session; the app's overview lists them to members.

// AuditTerminal is the action of a terminal session.
const AuditTerminal = "terminal"

// AuditEntry is one audit row.
type AuditEntry struct {
	ID           int64      `json:"id"`
	Action       string     `json:"action"`
	AppID        int64      `json:"app_id"`
	DeploymentID *int64     `json:"deployment_id,omitempty"`
	UserID       *int64     `json:"user_id,omitempty"`
	Actor        string     `json:"actor"`
	Process      string     `json:"process,omitempty"`
	Pod          string     `json:"pod,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	ExitCode     *int       `json:"exit_code,omitempty"`
	EndReason    string     `json:"end_reason,omitempty"`
	// CommitSHA of the deployment (read only, for listings).
	CommitSHA string `json:"commit_sha,omitempty"`
}

// StartAudit inserts an open entry (EndedAt unset) and returns its id.
// UserID 0 or nil is the admin principal.
func (s *Store) StartAudit(ctx context.Context, e AuditEntry) (int64, error) {
	var uid any
	if e.UserID != nil && *e.UserID > 0 {
		uid = *e.UserID
	}
	var dep any
	if e.DeploymentID != nil {
		dep = *e.DeploymentID
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO audit_log (action, app_id, deployment_id, user_id, actor, process, pod)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		e.Action, e.AppID, dep, uid, e.Actor, e.Process, e.Pod).Scan(&id)
	return id, err
}

// EndAudit closes an open entry; exitCode nil means none (the session was
// cut, or the shell never started). Closing twice keeps the first end.
func (s *Store) EndAudit(ctx context.Context, id int64, exitCode *int, reason string) error {
	var code any
	if exitCode != nil {
		code = *exitCode
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE audit_log SET ended_at = now(), exit_code = $2, end_reason = $3
		WHERE id = $1 AND ended_at IS NULL`, id, code, reason)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM audit_log WHERE id = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

// ListAudit returns an app's newest entries of action ("" for all).
func (s *Store) ListAudit(ctx context.Context, appID int64, action string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.id, l.action, l.app_id, l.deployment_id, l.user_id, l.actor, l.process, l.pod,
			l.started_at, l.ended_at, l.exit_code, l.end_reason, COALESCE(d.commit_sha, '')
		FROM audit_log l LEFT JOIN deployments d ON d.id = l.deployment_id
		WHERE l.app_id = $1 AND ($2 = '' OR l.action = $2)
		ORDER BY l.started_at DESC, l.id DESC
		LIMIT $3`, appID, action, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var dep, uid sql.NullInt64
		var code sql.NullInt32
		if err := rows.Scan(&e.ID, &e.Action, &e.AppID, &dep, &uid, &e.Actor, &e.Process, &e.Pod,
			&e.StartedAt, &e.EndedAt, &code, &e.EndReason, &e.CommitSHA); err != nil {
			return nil, err
		}
		if dep.Valid {
			e.DeploymentID = &dep.Int64
		}
		if uid.Valid {
			e.UserID = &uid.Int64
		}
		if code.Valid {
			c := int(code.Int32)
			e.ExitCode = &c
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
