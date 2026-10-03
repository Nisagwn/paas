package store

import "context"

// LatestDeployments returns the newest deployment of every app that has one,
// keyed by app id.
func (s *Store) LatestDeployments(ctx context.Context) (map[int64]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+deploymentCols+` FROM deployments d JOIN apps a ON a.id = d.app_id
		WHERE d.id IN (SELECT max(id) FROM deployments GROUP BY app_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out[d.AppID] = d
	}
	return out, rows.Err()
}

// Finished reports whether a deployment reached a final status.
func (d Deployment) Finished() bool {
	return d.Status == StatusReady || d.Status == StatusFailed
}
