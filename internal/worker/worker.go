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

// Notifier reports deployment progress to an outside system (GitHub commit
// statuses and PR comments). Calls are best effort: they run with their own
// short timeout and an error is logged, never fails the deployment.
type Notifier interface {
	DeploymentStarted(ctx context.Context, d store.Deployment) error
	DeploymentFinished(ctx context.Context, d store.Deployment, r Result) error
}

// Result is the outcome of a deployment as passed to a Notifier.
type Result struct {
	// Status is store.StatusReady or store.StatusFailed.
	Status string
	// Error is the failure reason; empty when ready.
	Error string
	// URL is the immutable deployment URL, PreviewURL the branch alias.
	// ProductionURL is set when the branch is the production branch.
	URL, PreviewURL, ProductionURL string
}

type Worker struct {
	Store    *store.Store
	Pipeline Pipeline
	Router   Router
	Domain   string
	// Scheme of app URLs in logs and notifications; empty means "https".
	Scheme       string
	PollInterval time.Duration
	Concurrency  int
	// Timeout bounds a whole deployment (build + deploy).
	Timeout time.Duration
	Log     *slog.Logger
	// Notifier is optional (nil: no notifications).
	Notifier Notifier
	// NotifyTimeout bounds each Notifier call (default 15s).
	NotifyTimeout time.Duration

	// Faz 7 (recovery.go). Cleanup is kicked when a deployment finishes,
	// so failed leftovers and deployments beyond retention go at once.
	Cleanup Kicker
	// StaleAfter: a building/deploying deployment without a heartbeat for
	// this long is an orphan of a dead worker and is recovered. Zero
	// disables heartbeats and recovery.
	StaleAfter time.Duration
	// MaxAttempts bounds claims of one deployment (DefaultMaxAttempts).
	MaxAttempts int
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
	if w.StaleAfter > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.recoverLoop(ctx)
		}()
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
	runCtx, cancelLease := context.WithCancelCause(runCtx)
	defer cancelLease(nil)
	defer w.kick(d.AppName)

	w.notify(ctx, d, func(nctx context.Context) error { return w.Notifier.DeploymentStarted(nctx, d) })

	stopHeartbeat := w.startHeartbeat(runCtx, d, cancelLease)
	err = w.run(runCtx, d)
	stopHeartbeat()
	if err != nil && errors.Is(context.Cause(runCtx), errLeaseLost) {
		// Another attempt owns the deployment now; it records the outcome.
		w.Log.Warn("deployment taken over", "deployment", d.ID, "app", d.AppName, "err", err)
		return true, nil
	}
	if err != nil {
		// runCtx may be the reason we failed (timeout), so record the failure
		// with a fresh context; otherwise the row would stay "building" forever.
		failCtx, cancelFail := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancelFail()
		w.Log.Warn("deployment failed", "deployment", d.ID, "app", d.AppName, "err", err)
		w.logLine(failCtx, d.ID, "ERROR: %v", err)
		mErr := w.Store.MarkFailed(failCtx, d.ID, err.Error())
		res := w.result(ctx, d, store.StatusFailed)
		res.Error = err.Error()
		w.notify(ctx, d, func(nctx context.Context) error { return w.Notifier.DeploymentFinished(nctx, d, res) })
		return true, mErr
	}
	w.Log.Info("deployment ready", "deployment", d.ID, "app", d.AppName,
		"url", naming.URL(w.Scheme, naming.DeploymentHost(d.CommitSHA, d.AppName, w.Domain)))
	res := w.result(ctx, d, store.StatusReady)
	if cur, err := w.Store.GetDeployment(ctx, d.ID); err == nil && cur.Status == store.StatusRetired {
		// The branch was deleted meanwhile: there is no preview to announce.
		res = w.result(ctx, d, store.StatusFailed)
		res.Error = "the branch was deleted during the deployment; it was retired"
	}
	w.notify(ctx, d, func(nctx context.Context) error { return w.Notifier.DeploymentFinished(nctx, d, res) })
	return true, nil
}

// result describes the outcome for the Notifier.
func (w *Worker) result(ctx context.Context, d store.Deployment, status string) Result {
	r := Result{
		Status:     status,
		URL:        naming.URL(w.Scheme, naming.DeploymentHost(d.CommitSHA, d.AppName, w.Domain)),
		PreviewURL: naming.URL(w.Scheme, naming.PreviewHost(d.Branch, d.AppName, w.Domain)),
	}
	if status == store.StatusReady && w.Notifier != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if app, err := w.Store.GetAppByName(ctx, d.AppName); err == nil && d.Branch == app.ProductionBranch {
			r.ProductionURL = naming.URL(w.Scheme, naming.ProductionHost(d.AppName, w.Domain))
		}
	}
	return r
}

// notify runs one Notifier call with its own timeout, detached from ctx so
// the final status is still reported during shutdown. Errors (and panics)
// are logged; they never change the deployment's outcome.
func (w *Worker) notify(ctx context.Context, d store.Deployment, call func(context.Context) error) {
	if w.Notifier == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			w.Log.Error("deployment notifier panicked", "deployment", d.ID, "panic", p)
		}
	}()
	timeout := w.NotifyTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := call(nctx); err != nil {
		w.Log.Warn("deployment notification failed", "deployment", d.ID, "app", d.AppName, "err", err)
		logCtx, cancelLog := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelLog()
		w.logLine(logCtx, d.ID, "WARNING: notification failed: %v", err)
	}
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
	if err := w.Store.MarkReady(ctx, d, aliases); errors.Is(err, store.ErrRetired) {
		log("==> branch was deleted during the deployment; retired without aliases")
		return nil
	} else if err != nil {
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
		log("==> %s alias: %s", a.Kind, naming.URL(w.Scheme, a.Hostname))
	}
	log("==> ready: %s", naming.URL(w.Scheme, naming.DeploymentHost(d.CommitSHA, d.AppName, w.Domain)))
	return nil
}

func (w *Worker) logLine(ctx context.Context, id int64, format string, args ...any) {
	if err := w.Store.AppendLog(ctx, id, fmt.Sprintf(format, args...)); err != nil {
		w.Log.Error("append log", "deployment", id, "err", err)
	}
}
