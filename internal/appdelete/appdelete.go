// Package appdelete deletes projects (Faz 28).
//
// A deletion has three steps; every step can be repeated, so a crash at
// any point leaves a record (store.AppDeletion) that the background loop
// (Service.Run) carries on from:
//
//  1. Begin (one transaction, store.BeginAppDeletion): the deletion is
//     recorded with who asked, the app is hidden (pushes, deploy hooks,
//     redeploys and the API see no app; nothing new is queued or claimed),
//     queued deployments are canceled and running ones are asked to stop
//     through the Faz 17 cancel machinery. A "project.deleted"
//     notification is emitted.
//  2. Rows (one transaction, store.DeleteAppRows), as soon as no deployment
//     is building or deploying any more (a running one stops at its next
//     heartbeat; one whose worker died is recovered as canceled): the app
//     row is deleted and everything referencing it cascades. Doing this
//     before the cluster step means no loop (add-ons, routes, domains,
//     rollouts) finds the app any more and recreates objects.
//  3. Cluster (Cluster.DeleteApp): the namespace and what lives outside it
//     are deleted; the deletion finishes once the namespace is gone. Only
//     then may a new project take the name (store.CreateAppInTeam), so it
//     never meets the old namespace. Faz 30: the project's images are
//     deleted from the registry in the same step, after the namespace.
//
// Delete runs the steps once inline: with nothing running and a cluster
// that removes the namespace at once (the dry-run deployer, tests) the
// project is gone when it returns (Result.Done); otherwise the loop
// finishes it within seconds and the caller answers 202.
package appdelete

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
)

// ErrConfirm is returned when the typed confirmation is not the app's name.
var ErrConfirm = errors.New("confirmation does not match the app name")

// Cluster removes a deleted app's objects (deploy.Kubernetes).
type Cluster interface {
	// DeleteApp deletes every object of the app and reports done once
	// nothing is left (the namespace may take a while to terminate). It
	// must be idempotent.
	DeleteApp(ctx context.Context, app string, appID int64) (done bool, err error)
}

// Images removes a deleted app's images from the registry
// (registry.Client). It must be idempotent.
type Images interface {
	DeleteAppImages(ctx context.Context, app string) (manifests int, err error)
}

// Service deletes projects. Store is required; the rest is optional.
type Service struct {
	Store *store.Store
	// Cluster is nil when nothing runs in a cluster (PAAS_DEPLOYER=dryrun).
	Cluster Cluster
	// Images is nil when no registry is pruned (dry-run builder,
	// PAAS_IMAGE_GC=false).
	Images Images
	// Notify receives the project.deleted event.
	Notify notify.Emitter
	Log    *slog.Logger
	// Interval of the background loop (default 5s); Lease bounds one
	// step run (default 2m).
	Interval, Lease time.Duration
	// StepTimeout bounds the inline run of Delete (default 20s).
	StepTimeout time.Duration
}

// Result is the outcome of Delete.
type Result struct {
	Deletion store.AppDeletion
	// Done: rows and cluster objects are gone; false means the background
	// loop finishes the deletion.
	Done bool
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

func (s *Service) lease() time.Duration {
	if s.Lease > 0 {
		return s.Lease
	}
	return 2 * time.Minute
}

// Delete deletes app after checking that confirm is its name. by is the
// caller (login and user id).
func (s *Service) Delete(ctx context.Context, app store.App, confirm string, by store.DeletionActor) (Result, error) {
	if confirm != app.Name {
		return Result{}, ErrConfirm
	}
	d, err := s.Store.BeginAppDeletion(ctx, app.ID, by)
	if err != nil {
		return Result{}, err
	}
	s.log().Info("project deleted", "app", app.Name, "app_id", app.ID, "team_id", app.TeamID,
		"repo", app.Repo, "by", by.Login, "deletion", d.ID)
	if s.Notify != nil {
		if err := s.Notify.Emit(ctx, notify.ProjectDeletedEvent(app, by.Login, d.ID)); err != nil {
			s.log().Warn("project deleted: notification", "app", app.Name, "err", err)
		}
	}
	// The steps outlive a client that disconnects; the loop retries anyway.
	timeout := s.StepTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	done, err := s.Advance(sctx, d)
	if err != nil {
		s.log().Warn("project deletion continues in the background", "app", app.Name, "err", err)
	}
	return Result{Deletion: d, Done: done}, nil
}

// Advance runs the remaining steps of d once and reports whether the
// deletion finished. Another runner holding the lease makes it a no-op.
func (s *Service) Advance(ctx context.Context, d store.AppDeletion) (bool, error) {
	ok, err := s.Store.ClaimAppDeletion(ctx, d.ID, s.lease())
	if err != nil || !ok {
		return false, err
	}
	done, err := s.advance(ctx, d)
	if done {
		if err := s.Store.FinishAppDeletion(ctx, d.ID); err != nil {
			return false, err
		}
		s.log().Info("project deletion finished", "app", d.AppName, "app_id", d.AppID, "deletion", d.ID)
		return true, nil
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if rerr := s.Store.ReleaseAppDeletion(context.WithoutCancel(ctx), d.ID, msg); rerr != nil && err == nil {
		err = rerr
	}
	return false, err
}

func (s *Service) advance(ctx context.Context, d store.AppDeletion) (bool, error) {
	if d.RowsDeletedAt == nil {
		err := s.Store.DeleteAppRows(ctx, d)
		if errors.Is(err, store.ErrDeploymentsRunning) {
			return false, nil // the next run checks again
		}
		if err != nil {
			return false, fmt.Errorf("delete rows: %w", err)
		}
	}
	if s.Cluster != nil {
		done, err := s.Cluster.DeleteApp(ctx, d.AppName, d.AppID)
		if err != nil {
			return false, fmt.Errorf("cluster: %w", err)
		}
		if !done {
			return false, nil
		}
	}
	// Faz 30: with no pod left, the project's images leave the registry
	// (before the name is free, so a new project never shares them).
	if s.Images != nil {
		n, err := s.Images.DeleteAppImages(ctx, d.AppName)
		if err != nil {
			return false, fmt.Errorf("images: %w", err)
		}
		if n > 0 {
			s.log().Info("project images deleted", "app", d.AppName, "manifests", n)
		}
	}
	return true, nil
}

// Run finishes pending deletions every Interval until ctx ends; it also
// picks up deletions a crashed control plane left behind.
func (s *Service) Run(ctx context.Context) {
	every := s.Interval
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep runs Advance for every pending deletion once.
func (s *Service) Sweep(ctx context.Context) {
	pending, err := s.Store.PendingAppDeletions(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log().Error("project deletions: list", "err", err)
		}
		return
	}
	for _, d := range pending {
		if _, err := s.Advance(ctx, d); err != nil && ctx.Err() == nil {
			s.log().Warn("project deletion: retrying", "app", d.AppName, "deletion", d.ID, "err", err)
		}
	}
}
