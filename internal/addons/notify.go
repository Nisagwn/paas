package addons

import (
	"context"
	"time"

	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
)

// notifyFailure emits addon.failed (Faz 26). key identifies the occurrence
// so a step repeated by the next pass or another replica is sent once.
// Best effort: an error is logged.
func (c *Controller) notifyFailure(ctx context.Context, a store.Addon, kind, detail, key string) {
	if c.Notify == nil {
		return
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.Notify.Emit(nctx, notify.AddonFailure(a, kind, detail, key)); err != nil {
		c.Log.Warn("addons: notification", "app", a.AppName, "addon", a.Name, "err", err)
	}
}
