package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Faz 11: scale to zero. The scaler (internal/scale) reads candidates here
// and mirrors the cluster's sleeping state back for the API and the UI.

// ScaleCandidate is a ready deployment the scaler may put to sleep.
type ScaleCandidate struct {
	DeploymentID  int64
	AppID         int64
	AppName       string
	CommitSHA     string
	FinishedAt    time.Time
	SleepingSince *time.Time
	// Production: the app's production alias points at this deployment.
	Production bool
	// ScaleProduction: the app lets its production deployment sleep too.
	ScaleProduction bool
}

// ScaleCandidates returns every ready deployment that has not been retired.
func (s *Store) ScaleCandidates(ctx context.Context) ([]ScaleCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.app_id, a.name, d.commit_sha, COALESCE(d.finished_at, d.created_at), d.sleeping_since,
			EXISTS (SELECT 1 FROM aliases al WHERE al.deployment_id = d.id AND al.kind = 'production'),
			a.scale_to_zero_production
		FROM deployments d JOIN apps a ON a.id = d.app_id
		WHERE d.status = 'ready' AND d.retired_at IS NULL
		ORDER BY d.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScaleCandidate{}
	for rows.Next() {
		var c ScaleCandidate
		if err := rows.Scan(&c.DeploymentID, &c.AppID, &c.AppName, &c.CommitSHA, &c.FinishedAt, &c.SleepingSince,
			&c.Production, &c.ScaleProduction); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetSleeping records that a deployment was scaled to zero (or woke up).
// Setting it again while asleep keeps the original time.
func (s *Store) SetSleeping(ctx context.Context, id int64, sleeping bool) error {
	q := `UPDATE deployments SET sleeping_since = NULL WHERE id = $1`
	if sleeping {
		q = `UPDATE deployments SET sleeping_since = COALESCE(sleeping_since, now()) WHERE id = $1`
	}
	_, err := s.db.ExecContext(ctx, q, id)
	return err
}

// ScaleToZeroProduction reports whether the app's production deployment
// may be scaled to zero.
func (s *Store) ScaleToZeroProduction(ctx context.Context, appID int64) (bool, error) {
	var on bool
	err := s.db.QueryRowContext(ctx, `SELECT scale_to_zero_production FROM apps WHERE id = $1`, appID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return on, err
}

// SetScaleToZeroProduction changes the app's production setting.
func (s *Store) SetScaleToZeroProduction(ctx context.Context, appID int64, on bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET scale_to_zero_production = $2 WHERE id = $1`, appID, on)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
