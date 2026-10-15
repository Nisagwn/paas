// Package cleanup retires deployments that are no longer needed and deletes
// their cluster objects, so old deployments do not fill the app's
// ResourceQuota (Faz 7).
//
// Policy, per app:
//   - a deployment an alias points at (production, preview) is never retired;
//   - production branch: the newest KeepProduction ready deployments stay as
//     rollback targets, older ones are retired;
//   - other branches: unaliased deployments are retired PreviewTTL after
//     they finished; preview aliases idle for PreviewAliasTTL are removed
//     first (optional), so whole stale previews go;
//   - deleted branch / closed pull request: handled at once by the webhook
//     (store.DeleteBranch), then Kick;
//   - failed deployments: their leftovers (e.g. a crash-looping pod) are
//     deleted right after the failure.
//
// Retirement is recorded in Postgres first and the cluster objects are
// deleted afterwards; a failed delete stays pending and is retried.
//
// Faz 30: once the cluster objects are gone, the deployment's image is
// deleted from the registry too, unless a deployment that is kept (live,
// rollback target, active preview, queued or running) references the same
// manifest (store.ReleaseImage decides per digest). Registry retention thus
// follows this policy instead of a blind image count; a failed registry
// delete keeps the deployment pending and is retried like a failed
// Kubernetes delete.
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nisagwn/paas/internal/registry"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

// Router pushes an app's aliases to the ingress layer (routing.Syncer).
type Router interface {
	SyncApp(ctx context.Context, app string) error
}

// Images deletes images from the registry the builder pushes to
// (registry.Client). Every method is idempotent.
type Images interface {
	// Owns reports whether img lives in this registry.
	Owns(img registry.Image) bool
	// Resolve returns img's digest ("" when its tag no longer exists).
	Resolve(ctx context.Context, img registry.Image) (string, error)
	// Delete deletes the manifest repository@digest and all its tags.
	Delete(ctx context.Context, repository, digest string) error
}

type Policy struct {
	// Ready production deployments kept as rollback targets.
	KeepProduction int
	// Unaliased preview deployments are retired this long after they
	// finished. Zero keeps them.
	PreviewTTL time.Duration
	// Preview aliases not moved for this long are removed. Zero keeps them.
	PreviewAliasTTL time.Duration
}

type Collector struct {
	Store *store.Store
	// Retirer deletes cluster objects; nil means there are none (dry run).
	Retirer worker.Retirer
	// Router is nil when nothing is routed (dry run).
	Router Router
	// Images is nil when no registry is pruned (dry-run builder, or
	// PAAS_IMAGE_GC=false).
	Images   Images
	Policy   Policy
	Interval time.Duration // of the full sweep in Run; zero means 10m
	Log      *slog.Logger

	kicks chan string
}

// Result counts what one sweep did.
type Result struct {
	AliasesExpired int
	Retired        int
	Cleaned        int
	Errors         int
}

func (r *Result) add(o Result) {
	r.AliasesExpired += o.AliasesExpired
	r.Retired += o.Retired
	r.Cleaned += o.Cleaned
	r.Errors += o.Errors
}

// New returns a collector ready to receive kicks before Run starts.
func New(st *store.Store, retirer worker.Retirer, router Router, p Policy, interval time.Duration, log *slog.Logger) *Collector {
	return &Collector{Store: st, Retirer: retirer, Router: router, Policy: p, Interval: interval, Log: log,
		kicks: make(chan string, 64)}
}

// Kick asks Run to sweep app soon. It never blocks: if the queue is full,
// the periodic sweep catches up.
func (c *Collector) Kick(app string) {
	if c.kicks == nil {
		return
	}
	select {
	case c.kicks <- app:
	default:
	}
}

// Run sweeps all apps at start and every Interval, and single apps when
// kicked, until ctx ends. Sweeps run one at a time.
func (c *Collector) Run(ctx context.Context) {
	interval := c.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	c.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.RunOnce(ctx)
		case name := <-c.kicks:
			app, err := c.Store.GetAppByName(ctx, name)
			if err != nil {
				c.Log.Error("cleanup: app", "app", name, "err", err)
				continue
			}
			c.logResult(app.Name, c.CollectApp(ctx, app, true))
		}
	}
}

// RunOnce sweeps every app.
func (c *Collector) RunOnce(ctx context.Context) Result {
	var total Result
	apps, err := c.Store.ListApps(ctx)
	if err != nil {
		c.Log.Error("cleanup: list apps", "err", err)
		total.Errors++
		return total
	}
	for _, a := range apps {
		r := c.CollectApp(ctx, a, false)
		c.logResult(a.Name, r)
		total.add(r)
	}
	return total
}

func (c *Collector) logResult(app string, r Result) {
	if r != (Result{}) {
		c.Log.Info("cleanup", "app", app, "aliases_expired", r.AliasesExpired,
			"retired", r.Retired, "cleaned", r.Cleaned, "errors", r.Errors)
	}
}

// CollectApp applies the policy to one app and deletes the cluster objects
// of its retired and failed deployments. syncRoutes forces an alias sync
// (aliases were removed elsewhere, e.g. by a branch deletion).
func (c *Collector) CollectApp(ctx context.Context, app store.App, syncRoutes bool) Result {
	var r Result
	fail := func(msg string, args ...any) {
		r.Errors++
		c.Log.Error("cleanup: "+msg, append([]any{"app", app.Name}, args...)...)
	}

	n, err := c.Store.ExpirePreviewAliases(ctx, app.ID, c.Policy.PreviewAliasTTL)
	if err != nil {
		fail("expire preview aliases", "err", err)
	}
	r.AliasesExpired = n

	candidates, err := c.Store.RetireCandidates(ctx, app.ID, c.Policy.KeepProduction, c.Policy.PreviewTTL)
	if err != nil {
		fail("retire candidates", "err", err)
	}
	for _, d := range candidates {
		reason := fmt.Sprintf("preview: no alias and finished more than %s ago", c.Policy.PreviewTTL)
		if d.Branch == app.ProductionBranch {
			reason = fmt.Sprintf("production: older than the newest %d ready deployments", c.Policy.KeepProduction)
		}
		ok, err := c.Store.Retire(ctx, d.ID, reason)
		if err != nil {
			fail("retire", "deployment", d.ID, "err", err)
			continue
		}
		if ok {
			r.Retired++
			c.logLine(ctx, d.ID, "==> retired: %s", reason)
		}
	}

	// Alias routes go before their backends, so no route is left pointing
	// at a deleted Service.
	if c.Router != nil && (syncRoutes || r.AliasesExpired > 0) {
		if err := c.Router.SyncApp(ctx, app.Name); err != nil {
			fail("route sync", "err", err)
		}
	}

	pending, err := c.Store.PendingCleanup(ctx, app.ID, 100)
	if err != nil {
		fail("pending cleanup", "err", err)
	}
	for _, d := range pending {
		if c.Retirer != nil {
			if err := c.Retirer.Retire(ctx, d); err != nil {
				fail("delete objects", "deployment", d.ID, "err", err)
				continue // retried on the next sweep
			}
		}
		// After the objects: no pod of d pulls the image any more.
		if err := c.deleteImage(ctx, d); err != nil {
			fail("delete image", "deployment", d.ID, "image", d.Image, "err", err)
			continue // retried on the next sweep
		}
		if err := c.Store.MarkCleaned(ctx, d.ID); err != nil {
			fail("mark cleaned", "deployment", d.ID, "err", err)
			continue
		}
		r.Cleaned++
	}
	return r
}

// deleteImage deletes d's image from the registry unless a kept deployment
// references the same manifest. Images the registry cannot delete (deletes
// disabled) or that live elsewhere are left alone and are not an error.
func (c *Collector) deleteImage(ctx context.Context, d store.Deployment) error {
	if c.Images == nil || d.Image == "" {
		return nil
	}
	img, err := registry.Parse(d.Image)
	if err != nil || !c.Images.Owns(img) {
		return nil // not pushed by this builder (dry run, an earlier registry)
	}
	digest, err := c.Images.Resolve(ctx, img)
	if err != nil {
		return err
	}
	if digest == "" {
		return nil // the tag is gone already
	}
	ok, err := c.Store.ReleaseImage(ctx, d.ID, img.Name(), digest, d.Image)
	if err != nil || !ok {
		return err // !ok: a kept deployment still runs this manifest
	}
	err = c.Images.Delete(ctx, img.Repository, digest)
	if errors.Is(err, registry.ErrUnsupported) {
		c.Log.Warn("cleanup: the registry does not allow deletes; image kept", "app", d.AppName,
			"deployment", d.ID, "image", d.Image, "err", err)
		return nil
	}
	if err != nil {
		return err
	}
	c.logLine(ctx, d.ID, "==> image deleted from the registry: %s", img.Name()+"@"+digest)
	return nil
}

func (c *Collector) logLine(ctx context.Context, id int64, format string, args ...any) {
	if err := c.Store.AppendLog(ctx, id, fmt.Sprintf(format, args...)); err != nil {
		c.Log.Error("cleanup: append log", "deployment", id, "err", err)
	}
}
