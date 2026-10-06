package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"github.com/nisagwn/paas/internal/process"
)

// Faz 20: process types (migration 013). A deployment's process set is
// written by the worker after the build and read by the deployer and the
// process reconcile (routing.Syncer); replica overrides are per app.

// SetProcesses records the process set a deployment was built with. A
// retried build overwrites it.
func (s *Store) SetProcesses(ctx context.Context, deploymentID int64, set process.Set) error {
	spec, err := json.Marshal(set)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO deployment_processes (deployment_id, spec, has_web) VALUES ($1, $2, $3)
		ON CONFLICT (deployment_id) DO UPDATE SET spec = EXCLUDED.spec, has_web = EXCLUDED.has_web`,
		deploymentID, spec, set.HasWeb())
	return err
}

// DeploymentProcesses returns the process set of a deployment; one built
// without process detection runs its web process only (process.Default).
func (s *Store) DeploymentProcesses(ctx context.Context, deploymentID int64) (process.Set, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT spec FROM deployment_processes WHERE deployment_id = $1`,
		deploymentID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return process.Default(), nil
	}
	if err != nil {
		return process.Set{}, err
	}
	return decodeSet(raw)
}

func decodeSet(raw []byte) (process.Set, error) {
	var set process.Set
	if err := json.Unmarshal(raw, &set); err != nil {
		return process.Set{}, fmt.Errorf("decode process set: %w", err)
	}
	return set, nil
}

// copyProcesses gives a redeploy or promotion that reuses src's image the
// process set of that image.
func (s *Store) copyProcesses(ctx context.Context, src, dst int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO deployment_processes (deployment_id, spec, has_web)
		SELECT $2, spec, has_web FROM deployment_processes WHERE deployment_id = $1
		ON CONFLICT (deployment_id) DO NOTHING`, src, dst)
	return err
}

// ProcessReplicas returns the app's replica overrides by process name.
func (s *Store) ProcessReplicas(ctx context.Context, appID int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT process, replicas FROM app_process_scale WHERE app_id = $1`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// SetProcessReplicas sets the app's replica override of a worker; nil
// removes it (paas.yaml decides again). Names and bounds are checked by the
// caller (process.ValidName, 0..process.MaxReplicas) and by the schema.
func (s *Store) SetProcessReplicas(ctx context.Context, appID int64, name string, replicas *int) error {
	var err error
	if replicas == nil {
		_, err = s.db.ExecContext(ctx, `DELETE FROM app_process_scale WHERE app_id = $1 AND process = $2`, appID, name)
	} else {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO app_process_scale (app_id, process, replicas) VALUES ($1, $2, $3)
			ON CONFLICT (app_id, process) DO UPDATE SET replicas = EXCLUDED.replicas, updated_at = now()`,
			appID, name, *replicas)
	}
	var pqErr *pq.Error
	switch {
	case errors.As(err, &pqErr) && pqErr.Code == "23503": // foreign_key_violation
		return ErrNotFound
	case errors.As(err, &pqErr) && pqErr.Code == "23514": // check_violation
		return ErrInvalid
	}
	return err
}

// AppProcesses returns what the process reconcile needs for one app: every
// deployment that may still have process objects in the cluster (not
// retired, not failed or canceled) with its role, and the app's replica
// overrides. Deployments without a process row have no objects to manage.
func (s *Store) AppProcesses(ctx context.Context, appID int64) ([]process.Deployment, map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.status,
			EXISTS (SELECT 1 FROM aliases al WHERE al.deployment_id = d.id AND al.kind = 'production'),
			-- Only a preview-environment deployment runs as a preview: the production
			-- branch's own preview alias must not start a second production generation.
			d.target = 'preview' AND EXISTS (SELECT 1 FROM aliases al WHERE al.deployment_id = d.id AND al.kind = 'preview'),
			p.spec
		FROM deployments d JOIN deployment_processes p ON p.deployment_id = d.id
		WHERE d.app_id = $1 AND d.retired_at IS NULL
		  AND d.status IN ('queued', 'building', 'deploying', 'ready')
		ORDER BY d.id`, appID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []process.Deployment
	for rows.Next() {
		var d process.Deployment
		var status string
		var raw []byte
		if err := rows.Scan(&d.ID, &status, &d.Production, &d.Preview, &raw); err != nil {
			return nil, nil, err
		}
		if d.Set, err = decodeSet(raw); err != nil {
			return nil, nil, fmt.Errorf("deployment %d: %w", d.ID, err)
		}
		d.Ready = status == StatusReady
		d.InFlight = status == StatusQueued || status == StatusBuilding || status == StatusDeploying
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	overrides, err := s.ProcessReplicas(ctx, appID)
	return out, overrides, err
}

// WeblessDeployments returns the ids of the app's deployments that have no
// web process, so the alias router does not point an Ingress at a Service
// that does not exist.
func (s *Store) WeblessDeployments(ctx context.Context, appID int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id FROM deployments d JOIN deployment_processes p ON p.deployment_id = d.id
		WHERE d.app_id = $1 AND NOT p.has_web AND d.retired_at IS NULL`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ProductionAliasTarget returns the deployment the app's production alias
// points at (ErrNotFound when there is none yet).
func (s *Store) ProductionAliasTarget(ctx context.Context, appID int64) (Deployment, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT deployment_id FROM aliases WHERE app_id = $1 AND kind = $2`, appID, AliasProduction).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	return s.GetDeployment(ctx, id)
}
