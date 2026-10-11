package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 23: the team plans in the worker (plans.Guard).
//
//   - Fair queue: a team building as many deployments as its plan allows
//     at once is skipped by the claim; other teams' deployments go ahead.
//   - Build timeout: the build stage gets the plan's timeout (on top of
//     the deployment timeout, Worker.Timeout).
//   - Build minutes: the build stage's duration is stored per deployment
//     (every attempt adds); the monthly sum is checked when deploying.
//   - Process limits: a paas.yaml asking for more cron jobs or worker
//     replicas than the plan allows fails the deployment.

// Plans is the plan side of the worker (plans.Guard).
type Plans interface {
	ClaimCaps() store.ClaimCaps
	BuildTimeout(ctx context.Context, appID int64) (time.Duration, error)
	CheckProcesses(ctx context.Context, appID int64, set process.Set) error
}

// errBuildTimeout is the cause of a build stopped by the plan's timeout.
var errBuildTimeout = errors.New("build timeout of the plan")

func (w *Worker) claim(ctx context.Context) (store.Deployment, error) {
	if w.Plans == nil {
		return w.Store.ClaimNext(ctx)
	}
	return w.Store.ClaimNextFor(ctx, w.Plans.ClaimCaps())
}

// build runs the build stage under the plan's timeout and records its
// duration.
func (w *Worker) build(ctx context.Context, d store.Deployment, settings store.BuildSettings, log Logger) (BuildResult, error) {
	bctx := ctx
	var limit time.Duration
	if w.Plans != nil {
		var err error
		if limit, err = w.Plans.BuildTimeout(ctx, d.AppID); err != nil {
			return BuildResult{}, fmt.Errorf("plan: %w", err)
		}
		if limit > 0 {
			var cancel context.CancelFunc
			bctx, cancel = context.WithTimeoutCause(ctx, limit, errBuildTimeout)
			defer cancel()
		}
	}
	start := time.Now()
	built, err := w.Pipeline.Build(bctx, d, settings, log)
	secs := int(time.Since(start).Round(time.Second) / time.Second)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	if rerr := w.Store.AddBuildSeconds(rctx, d.ID, secs); rerr != nil {
		w.Log.Error("record build time", "deployment", d.ID, "err", rerr)
	}
	cancel()
	if err != nil && errors.Is(context.Cause(bctx), errBuildTimeout) {
		return built, fmt.Errorf("the build exceeded the plan's build timeout of %s", limit)
	}
	return built, err
}

// checkProcesses applies the plan to a built process set.
func (w *Worker) checkProcesses(ctx context.Context, d store.Deployment, set process.Set) error {
	if w.Plans == nil {
		return nil
	}
	return w.Plans.CheckProcesses(ctx, d.AppID, set)
}
