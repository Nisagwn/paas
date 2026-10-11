package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

// DeploymentEvents turns the worker's notifications into events. It is a
// worker.Notifier (and a rollout.Notifier), chained next to the GitHub
// notifier with Chain, so the worker and the rollout controller report to
// both through the call sites they already have.
type DeploymentEvents struct {
	Emitter Emitter
	Store   *store.Store
	Log     *slog.Logger
}

var _ worker.Notifier = (*DeploymentEvents)(nil)

// DeploymentStarted is not an event: only outcomes are notified.
func (n *DeploymentEvents) DeploymentStarted(context.Context, store.Deployment) error { return nil }

// DeploymentFinished emits deployment.ready / failed / canceled and, for a
// promotion that took the production alias, production.changed.
func (n *DeploymentEvents) DeploymentFinished(ctx context.Context, d store.Deployment, r worker.Result) error {
	err := n.Emitter.Emit(ctx, Deployment(d, r))
	if r.Status == store.StatusReady && d.Origin == store.OriginPromote && n.Store != nil {
		if prod, perr := n.productionIs(ctx, d); perr != nil {
			err = errors.Join(err, perr)
		} else if prod {
			err = errors.Join(err, n.Emitter.Emit(ctx, ProductionChange(d, ReasonPromote, "", fmt.Sprintf("promote:%d", d.ID))))
		}
	}
	return err
}

// productionIs reports whether the app's production alias points at d (a
// canary keeps it on the previous deployment for now).
func (n *DeploymentEvents) productionIs(ctx context.Context, d store.Deployment) (bool, error) {
	aliases, err := n.Store.ListAliases(ctx, d.AppID)
	if err != nil {
		return false, err
	}
	for _, a := range aliases {
		if a.Kind == store.AliasProduction {
			return a.DeploymentID == d.ID, nil
		}
	}
	return false, nil
}

// RolloutFinished emits the outcome of a rollout the controller decided:
// rollout.rolled_back for an automatic rollback or abort,
// production.changed for a completed canary.
func (n *DeploymentEvents) RolloutFinished(ctx context.Context, d store.Deployment, r store.Rollout) error {
	e, ok := Rollout(r, d)
	if !ok {
		return nil
	}
	return n.Emitter.Emit(ctx, e)
}

// RolloutNotifier is rollout.Notifier (not imported: the rollout package
// pulls in the Kubernetes client).
type RolloutNotifier interface {
	RolloutFinished(ctx context.Context, d store.Deployment, r store.Rollout) error
}

// Chain calls every notifier in order (nil ones are dropped) and joins
// their errors. It forwards RolloutFinished to the ones that implement it.
func Chain(ns ...worker.Notifier) worker.Notifier {
	var c chain
	for _, n := range ns {
		if n != nil {
			c = append(c, n)
		}
	}
	return c
}

type chain []worker.Notifier

func (c chain) DeploymentStarted(ctx context.Context, d store.Deployment) error {
	var errs []error
	for _, n := range c {
		errs = append(errs, n.DeploymentStarted(ctx, d))
	}
	return errors.Join(errs...)
}

func (c chain) DeploymentFinished(ctx context.Context, d store.Deployment, r worker.Result) error {
	var errs []error
	for _, n := range c {
		errs = append(errs, n.DeploymentFinished(ctx, d, r))
	}
	return errors.Join(errs...)
}

func (c chain) RolloutFinished(ctx context.Context, d store.Deployment, r store.Rollout) error {
	var errs []error
	for _, n := range c {
		if rn, ok := n.(RolloutNotifier); ok {
			errs = append(errs, rn.RolloutFinished(ctx, d, r))
		}
	}
	return errors.Join(errs...)
}

// EmitProductionChange emits production.changed for the deployment the
// production alias now points at (rollback, promote by alias, a user's
// rollout action). Best effort: a failure is logged, never returned, so
// the user's action stands. em nil is a no-op.
func EmitProductionChange(ctx context.Context, em Emitter, st *store.Store, log *slog.Logger, deploymentID int64, reason, by string) {
	if em == nil || deploymentID == 0 {
		return
	}
	d, err := st.GetDeployment(ctx, deploymentID)
	if err == nil {
		err = em.Emit(ctx, ProductionChange(d, reason, by, ""))
	}
	if err != nil && log != nil {
		log.Warn("notification: production change", "deployment", deploymentID, "err", err)
	}
}
