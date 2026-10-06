package routing

import (
	"context"
	"fmt"

	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 20: process types. Aliases decide which deployment's workers and
// crons run (process.PlanFor), so they are reconciled together with the
// routes: after MarkReady, after a rollback or promotion, after a replica
// change and periodically. A rollback therefore moves the workers and
// crons back to the old deployment as well.

// ProcessApplier makes the cluster's workers and crons match a plan
// (deploy.Kubernetes). SyncApp uses it when the Applier implements it.
type ProcessApplier interface {
	ApplyProcesses(ctx context.Context, app string, plan process.Plan) error
}

// webRoutes drops the routes of deployments without a web process: they
// have no Service to send traffic to. Their aliases stay in the database,
// so a rollback to such a deployment works like any other.
func (s *Syncer) webRoutes(ctx context.Context, appID int64, routes []store.AliasRoute) ([]store.AliasRoute, error) {
	webless, err := s.Store.WeblessDeployments(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("deployments without web: %w", err)
	}
	if len(webless) == 0 {
		return routes, nil
	}
	out := routes[:0:0]
	for _, r := range routes {
		if !webless[r.DeploymentID] {
			out = append(out, r)
		}
	}
	return out, nil
}

// applyProcesses runs the processes of the deployments the aliases point
// at and stops all others.
func (s *Syncer) applyProcesses(ctx context.Context, app store.App) error {
	pa, ok := s.Applier.(ProcessApplier)
	if !ok {
		return nil
	}
	deps, overrides, err := s.Store.AppProcesses(ctx, app.ID)
	if err != nil {
		return fmt.Errorf("processes: %w", err)
	}
	if err := pa.ApplyProcesses(ctx, app.Name, process.PlanFor(deps, overrides)); err != nil {
		return fmt.Errorf("processes: %w", err)
	}
	return nil
}
