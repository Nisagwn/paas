// Package rollout drives canary and guarded rollouts (Faz 21).
//
// MarkReady opens a rollout when a production deployment of an app in
// canary or guarded mode becomes ready (store/rollouts.go). Every Interval
// the Controller evaluates the running rollouts from the per-minute request
// metrics of Faz 19 (request_metrics): the new deployment against the
// current production over the current step's window (canary), or against
// the previous production's traffic before the switch (guard). Decide is
// the policy; the Controller applies its verdict:
//
//	advance   next weight (store), routing.Syncer applies the TraefikService weights
//	promote   canary: the production alias moves to the new deployment (the
//	          same alias row Rollback moves, so manual rollback keeps working);
//	          guard: the watch ends
//	rollback  canary: weight 0, the new deployment stays reachable on its own
//	          URL; guard: the production alias moves back to the previous one
//	abort     metrics missing for too long: production is left as it is
//
// Several control plane replicas may run the Controller: each evaluation
// claims the rollout (store.ClaimDueRollouts, FOR UPDATE SKIP LOCKED) and
// every transition is a compare-and-set, so a user action or a newer
// deployment that got there first wins and the stale decision is dropped.
package rollout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/nisagwn/paas/internal/analytics"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/store"
)

// DefaultInterval between evaluations.
const DefaultInterval = 30 * time.Second

// Router applies an app's routes and traffic split (routing.Syncer).
type Router interface {
	SyncApp(ctx context.Context, app string) error
}

// Notifier reports a finished rollout outside (GitHub commit status). Best
// effort: errors are logged.
type Notifier interface {
	RolloutFinished(ctx context.Context, d store.Deployment, r store.Rollout) error
}

type Controller struct {
	Store *store.Store
	// Router is nil without a cluster (dry run): weights are only recorded.
	Router   Router
	Notifier Notifier
	Interval time.Duration
	Log      *slog.Logger

	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
}

func (c *Controller) defaults() {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Run evaluates every Interval until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	c.defaults()
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		c.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick evaluates the rollouts that are due and returns how many it decided
// on (any action but hold).
func (c *Controller) Tick(ctx context.Context) int {
	c.defaults()
	// A little under the interval, so a replica's own next tick finds the
	// rollout due again while another replica's tick in between does not.
	due, err := c.Store.ClaimDueRollouts(ctx, c.Interval*4/5)
	if err != nil {
		c.Log.Error("rollout: claim", "err", err)
		return 0
	}
	n := 0
	for _, r := range due {
		acted, err := c.Evaluate(ctx, r)
		if err != nil {
			c.Log.Error("rollout: evaluate", "rollout", r.ID, "app", r.AppName, "err", err)
		}
		if acted {
			n++
		}
	}
	return n
}

// window is a range of whole minutes of request_metrics.
type window struct{ from, to time.Time }

// minutesBetween is the whole minutes in [from, to): the minute a step
// started in is partly the previous weight's, and the current minute is
// not collected yet.
func minutesBetween(from, to time.Time) window {
	f := from.UTC().Truncate(time.Minute)
	if !f.Equal(from.UTC()) {
		f = f.Add(time.Minute)
	}
	t := to.UTC().Truncate(time.Minute)
	if t.Before(f) {
		t = f
	}
	return window{f, t}
}

func (c *Controller) sample(ctx context.Context, depID int64, w window) (store.RolloutSample, error) {
	if depID == 0 || !w.to.After(w.from) {
		return store.RolloutSample{}, nil
	}
	p, err := c.Store.WindowTraffic(ctx, depID, w.from, w.to)
	if err != nil {
		return store.RolloutSample{}, err
	}
	return Sample(p), nil
}

// Sample turns summed request metrics into a verdict sample.
func Sample(p store.MetricPoint) store.RolloutSample {
	s := store.RolloutSample{Requests: p.Requests, Errors: p.Classes[3]}
	if q, ok := analytics.Quantile(0.95, p.Buckets); ok && p.Requests > 0 {
		ms := math.Round(q*1e4) / 10 // 0.1 ms resolution
		s.P95Ms = &ms
	}
	return s
}

// Input gathers the metrics of r for Decide at now.
func (c *Controller) Input(ctx context.Context, r store.Rollout, now time.Time) (Input, error) {
	in := Input{Mode: r.Mode, Settings: r.Settings, Elapsed: now.Sub(r.StepStartedAt)}
	var from int64
	if r.FromDeploymentID != nil {
		from = *r.FromDeploymentID
	}
	var err error
	cur := minutesBetween(r.StepStartedAt, now)
	// The production's traffic before the rollout: the guard's baseline,
	// and for both modes the hint whether traffic is expected at all.
	span := r.Settings.StepDuration()
	if r.Mode == store.RolloutGuarded {
		span = r.Settings.GuardDuration()
	}
	before := minutesBetween(r.CreatedAt.Add(-span), r.CreatedAt.Truncate(time.Minute))
	ref, err := c.sample(ctx, from, before)
	if err != nil {
		return in, err
	}
	in.ExpectTraffic = ref.Requests >= int64(r.Settings.MinRequests)
	if in.Canary, err = c.sample(ctx, r.ToDeploymentID, cur); err != nil {
		return in, err
	}
	if r.Mode == store.RolloutGuarded {
		in.Duration, in.LastStep, in.Baseline = r.Settings.GuardDuration(), true, ref
		return in, nil
	}
	in.Duration, in.LastStep = r.Settings.StepDuration(), r.LastStep()
	in.Baseline, err = c.sample(ctx, from, cur)
	return in, err
}

// Evaluate decides on one rollout and applies the verdict. It reports
// whether anything but hold happened.
func (c *Controller) Evaluate(ctx context.Context, r store.Rollout) (bool, error) {
	c.defaults()
	if aborted, err := c.ensureSplit(ctx, r); aborted || err != nil {
		return aborted, err
	}
	in, err := c.Input(ctx, r, c.Now())
	if err != nil {
		return false, err
	}
	v := Decide(in)
	verdict := &store.RolloutVerdict{
		At: c.Now().UTC(), Step: r.Step, Weight: r.Weight, Action: string(v.Action), Reason: v.Reason,
		Canary: &in.Canary, Baseline: &in.Baseline,
	}
	if v.Action == Hold {
		if v.Reason != r.Reason {
			_, err := c.Store.UpdateRollout(ctx, r, store.RolloutChange{Reason: v.Reason})
			if errors.Is(err, store.ErrStale) {
				err = nil
			}
			return false, err
		}
		return false, nil
	}
	ch := store.RolloutChange{Reason: v.Reason, Verdict: verdict}
	var logLine string
	switch v.Action {
	case Advance:
		step := r.Step + 1
		weight := r.Settings.Steps[step]
		ch.Step, ch.Weight, ch.RestartStep = &step, &weight, true
		verdict.Weight = weight
		logLine = fmt.Sprintf("==> canary: %%%d trafik (adım %d/%d; %s)", weight, step+1, len(r.Settings.Steps), v.Reason)
	case Promote:
		ch.State = store.RolloutPromoted
		if r.Mode == store.RolloutCanary {
			w := 100
			ch.Weight, ch.MoveProductionTo = &w, r.ToDeploymentID
			verdict.Weight = w
			logLine = "==> canary tamamlandı: production artık bu deploy (" + v.Reason + ")"
		} else {
			logLine = "==> izleme tamamlandı: production bu deploy'da kalıyor (" + v.Reason + ")"
		}
	case Rollback:
		ch.State = store.RolloutRolledBack
		w := 0
		ch.Weight = &w
		verdict.Weight = 0
		if r.Mode == store.RolloutGuarded {
			if r.FromDeploymentID == nil {
				return c.abort(ctx, r, verdict, v.Reason+"; önceki deploy artık yok, production değişmedi")
			}
			ch.MoveProductionTo = *r.FromDeploymentID
			logLine = fmt.Sprintf("==> otomatik geri alındı: production #%d deploy'una döndü (%s)", *r.FromDeploymentID, v.Reason)
		} else {
			logLine = "==> canary geri alındı: trafik %0 (" + v.Reason + "); deploy kendi adresinde açık kalır"
		}
	case Abort:
		return c.abort(ctx, r, verdict, v.Reason)
	}
	next, err := c.Store.UpdateRollout(ctx, r, ch)
	if errors.Is(err, store.ErrStale) {
		return false, nil // a user, a newer deployment or another replica decided first
	}
	if r.Mode == store.RolloutGuarded && ch.MoveProductionTo != 0 &&
		(errors.Is(err, store.ErrRetired) || errors.Is(err, store.ErrNotReady) || errors.Is(err, store.ErrNotFound)) {
		return c.abort(ctx, r, verdict, v.Reason+"; önceki deploy artık çalışmıyor, production değişmedi")
	}
	if err != nil {
		return false, err
	}
	c.finish(ctx, next, logLine)
	return true, nil
}

// abort ends r without moving production.
func (c *Controller) abort(ctx context.Context, r store.Rollout, verdict *store.RolloutVerdict, reason string) (bool, error) {
	ch := store.RolloutChange{State: store.RolloutAborted, Reason: reason, Verdict: verdict}
	verdict.Action, verdict.Reason = string(Abort), reason
	if r.Mode == store.RolloutCanary {
		w := 0
		ch.Weight, verdict.Weight = &w, 0
	}
	next, err := c.Store.UpdateRollout(ctx, r, ch)
	if errors.Is(err, store.ErrStale) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	c.finish(ctx, next, "==> yayın durduruldu: "+reason)
	return true, nil
}

// finish logs, applies the routes and, for a final state, notifies.
func (c *Controller) finish(ctx context.Context, r store.Rollout, logLine string) {
	c.Log.Info("rollout", "rollout", r.ID, "app", r.AppName, "state", r.State, "step", r.Step,
		"weight", r.Weight, "reason", r.Reason)
	if err := c.Store.AppendLog(ctx, r.ToDeploymentID, logLine); err != nil {
		c.Log.Error("rollout: append log", "rollout", r.ID, "err", err)
	}
	if err := c.Sync(ctx, r); err != nil {
		c.Log.Error("rollout: route sync", "rollout", r.ID, "app", r.AppName, "err", err)
	}
	if !r.Active() {
		c.notify(ctx, r)
	}
}

// Sync applies the app's routes after a rollout change.
func (c *Controller) Sync(ctx context.Context, r store.Rollout) error {
	if c.Router == nil {
		return nil
	}
	return c.Router.SyncApp(ctx, r.AppName)
}

// ensureSplit applies a canary's traffic split before its metrics are
// judged: a canary whose split is not in place received no traffic, and
// its empty metrics would look like a quiet, healthy app. A cluster that
// cannot split at all (no Traefik CRDs) aborts the rollout, production
// staying on the current deployment; any other routing error holds it.
func (c *Controller) ensureSplit(ctx context.Context, r store.Rollout) (aborted bool, err error) {
	if c.Router == nil || r.Mode != store.RolloutCanary {
		return false, nil
	}
	err = c.Router.SyncApp(ctx, r.AppName)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, deploy.ErrTraefikCRD) {
		return false, fmt.Errorf("traffic split not applied, holding: %w", err)
	}
	w := 0
	verdict := &store.RolloutVerdict{At: c.Now().UTC(), Step: r.Step, Weight: w}
	_, aerr := c.abort(ctx, r, verdict, "trafik bölünemiyor: "+err.Error())
	return true, aerr
}

func (c *Controller) notify(ctx context.Context, r store.Rollout) {
	if c.Notifier == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			c.Log.Error("rollout notifier panicked", "rollout", r.ID, "panic", p)
		}
	}()
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	d, err := c.Store.GetDeployment(nctx, r.ToDeploymentID)
	if err == nil {
		err = c.Notifier.RolloutFinished(nctx, d, r)
	}
	if err != nil {
		c.Log.Warn("rollout notification failed", "rollout", r.ID, "app", r.AppName, "err", err)
	}
}
