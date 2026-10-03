// Package worker pulls queued deployments and drives them through the pipeline:
//
//	queued → building → deploying → ready | failed
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// Logger lets pipeline stages write lines to the deployment's log.
type Logger func(format string, args ...any)

// Builder clones the commit and produces a container image (Faz 2).
type Builder interface {
	Build(ctx context.Context, d store.Deployment, log Logger) (image string, err error)
}

// Deployer creates the Kubernetes objects and waits until healthy (Faz 3).
type Deployer interface {
	Deploy(ctx context.Context, d store.Deployment, image string, log Logger) error
}

// Pipeline turns a commit into a running deployment.
type Pipeline interface {
	Builder
	Deployer
}

// Stages combines a Builder and a Deployer from different implementations,
// e.g. a real build with a dry-run deploy while Faz 3 is not done.
type Stages struct {
	Builder
	Deployer
}

// Router pushes an app's aliases from the database to the ingress layer
// (routing.Syncer). Nil when there is nothing to route (dry run).
type Router interface {
	SyncApp(ctx context.Context, app string) error
}

type Worker struct {
	Store        *store.Store
	Pipeline     Pipeline
	Router       Router
	Domain       string
	PollInterval time.Duration
	Concurrency  int
	// Timeout bounds a whole deployment (build + deploy).
	Timeout time.Duration
	Log     *slog.Logger
}

// Run starts Concurrency polling loops and blocks until ctx is cancelled and
// all in-flight deployments have finished.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < w.Concurrency; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.loop(ctx, n)
		}(i)
	}
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context, n int) {
	log := w.Log.With("worker", n)
	for {
		// Drain the queue before sleeping again.
		for ctx.Err() == nil {
			worked, err := w.ProcessOne(ctx)
			if err != nil {
				log.Error("process deployment", "err", err)
				break
			}
			if !worked {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.PollInterval):
		}
	}
}

// ProcessOne claims and runs a single deployment. It reports false when the
// queue was empty. Exported so tests can drive the worker step by step.
func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	d, err := w.Store.ClaimNext(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// A deployment that has started should finish even during shutdown,
	// so it gets its own context detached from ctx's cancellation.
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	if err := w.run(runCtx, d); err != nil {
		// runCtx may be the reason we failed (timeout), so record the failure
		// with a fresh context; otherwise the row would stay "building" forever.
		failCtx, cancelFail := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancelFail()
		w.Log.Warn("deployment failed", "deployment", d.ID, "app", d.AppName, "err", err)
		w.logLine(failCtx, d.ID, "ERROR: %v", err)
		if mErr := w.Store.MarkFailed(failCtx, d.ID, err.Error()); mErr != nil {
			return true, mErr
		}
		return true, nil
	}
	w.Log.Info("deployment ready", "deployment", d.ID, "app", d.AppName,
		"url", "https://"+naming.DeploymentHost(d.CommitSHA, d.AppName, w.Domain))
	return true, nil
}

func (w *Worker) run(ctx context.Context, d store.Deployment) error {
	log := func(format string, args ...any) { w.logLine(ctx, d.ID, format, args...) }
	log("==> deployment #%d: %s@%s (%s)", d.ID, d.AppName, naming.ShortSHA(d.CommitSHA), d.Branch)

	image, err := w.Pipeline.Build(ctx, d, log)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if err := w.Store.SetImage(ctx, d.ID, image); err != nil {
		return err
	}

	if err := w.Store.SetStatus(ctx, d.ID, store.StatusDeploying); err != nil {
		return err
	}
	if err := w.Pipeline.Deploy(ctx, d, image, log); err != nil {
		return fmt.Errorf("deploy: %w", err)
	}

	app, err := w.Store.GetAppByName(ctx, d.AppName)
	if err != nil {
		return err
	}
	aliases := []store.AliasSpec{{
		Hostname: naming.PreviewHost(d.Branch, d.AppName, w.Domain),
		Kind:     store.AliasPreview,
		Branch:   d.Branch,
	}}
	if d.Branch == app.ProductionBranch {
		aliases = append(aliases, store.AliasSpec{
			Hostname: naming.ProductionHost(d.AppName, w.Domain),
			Kind:     store.AliasProduction,
			Branch:   d.Branch,
		})
	}
	if err := w.Store.MarkReady(ctx, d, aliases); err != nil {
		return err
	}
	if w.Router != nil {
		// The deployment is up and reachable on its own URL either way; a
		// failed alias sync is retried by the router's reconcile loop.
		if err := w.Router.SyncApp(ctx, d.AppName); err != nil {
			log("WARNING: updating alias routes failed (retried automatically): %v", err)
		}
	}
	for _, a := range aliases {
		log("==> %s alias: https://%s", a.Kind, a.Hostname)
	}
	log("==> ready: https://%s", naming.DeploymentHost(d.CommitSHA, d.AppName, w.Domain))
	return nil
}

func (w *Worker) logLine(ctx context.Context, id int64, format string, args ...any) {
	if err := w.Store.AppendLog(ctx, id, fmt.Sprintf(format, args...)); err != nil {
		w.Log.Error("append log", "deployment", id, "err", err)
	}
}
