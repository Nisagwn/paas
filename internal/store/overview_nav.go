package store

import (
	"context"

	"github.com/lib/pq"
)

// Dashboard overview (web UI "Projeler" page and its onboarding checklist).

// ProductionDeployments returns, keyed by app id, the deployment each app's
// production alias points at. Apps without a production deployment are
// missing from the map.
func (s *Store) ProductionDeployments(ctx context.Context) (map[int64]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+deploymentCols+` FROM aliases al
		JOIN deployments d ON d.id = al.deployment_id
		JOIN apps a ON a.id = d.app_id
		WHERE al.kind = $1`, AliasProduction)
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

// TeamsHaveInstallation reports whether any of the teams has claimed a
// GitHub App installation that is not suspended.
func (s *Store) TeamsHaveInstallation(ctx context.Context, teamIDs []int64) (bool, error) {
	if len(teamIDs) == 0 {
		return false, nil
	}
	var ok bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM github_installations WHERE team_id = ANY($1) AND NOT suspended)`,
		pq.Array(teamIDs)).Scan(&ok)
	return ok, err
}
