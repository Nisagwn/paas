package addons

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

// The worker side (worker.Databases): a deployment asks for its databases
// before the build and waits for them before the deploy. The log lines
// (in the UI's language) say what happens:
//
//	==> veritabanı kopyası: db → preview_feature_x_1a2b3c4d (production'dan kopyalanıyor)
//	==> veritabanı kopyası: db hazır (12.3 MiB, anlık görüntü 05:27:04 UTC, 2 anonimleştirme kuralı)

var _ worker.Databases = (*Controller)(nil)

// Request creates the branch database rows of a preview deployment (or
// queues a failed copy again: every new deployment retries once) and kicks
// the reconcile loop, so the copy runs while the image builds.
func (c *Controller) Request(ctx context.Context, d store.Deployment, log worker.Logger) error {
	c.defaults()
	addons, err := c.Store.ListAddons(ctx, d.AppID)
	if err != nil || len(addons) == 0 {
		return err
	}
	for _, a := range addons {
		if a.Status == store.AddonDeleting {
			continue
		}
		if d.Target != store.EnvPreview || !a.HasBranches() {
			continue
		}
		b, err := c.Store.EnsureAddonBranch(ctx, a, d.Branch)
		if err != nil {
			return fmt.Errorf("branch database of add-on %s: %w", a.Name, err)
		}
		switch b.Status {
		case store.BranchReady:
			log("==> veritabanı kopyası: %s → %s (mevcut kopya kullanılıyor%s; yenilemek için \"Kopyayı yenile\")",
				a.Name, b.Database, snapshotNote(b))
		case store.BranchPending, store.BranchCopying:
			log("==> veritabanı kopyası: %s → %s (%s)", a.Name, b.Database, modeNote(b.Mode, a))
		}
	}
	c.Kick(d.AppID)
	return nil
}

// Wait returns when every add-on the deployment connects to is ready: the
// add-on itself and, for a preview, its branch database. A failed add-on
// or copy fails the deployment with the reason; so does ctx (the
// deployment's timeout).
func (c *Controller) Wait(ctx context.Context, d store.Deployment, log worker.Logger) error {
	c.defaults()
	said := map[string]string{} // add-on → last state logged
	say := func(key, state, format string, args ...any) {
		if said[key] != state {
			said[key] = state
			log(format, args...)
		}
	}
	start := c.Now()
	for {
		addons, err := c.Store.ListAddons(ctx, d.AppID)
		if err != nil {
			return err
		}
		waiting := false
		for _, a := range addons {
			switch a.Status {
			case store.AddonDeleting:
				continue
			case store.AddonFailed:
				return fmt.Errorf("add-on %s is not working: %s", a.Name, a.Message)
			case store.AddonProvisioning:
				waiting = true
				say(a.Name, "provisioning", "==> veritabanı hazırlanıyor: %s (%s, %s plan)", a.Name, a.Kind, a.Plan)
				continue
			}
			if said[a.Name] == "provisioning" {
				say(a.Name, "ready", "==> veritabanı hazır: %s", a.Name)
			}
			if d.Target != store.EnvPreview || a.Kind != store.AddonPostgres {
				continue
			}
			if !a.HasBranches() {
				say(a.Name, "shared", "==> veritabanı: %s, bu önizleme production veritabanını kullanıyor (önizleme modu: shared)", a.Name)
				continue
			}
			b, err := c.Store.GetAddonBranch(ctx, a.ID, d.Branch)
			if errors.Is(err, store.ErrNotFound) {
				// Dropped meanwhile (the branch's previews had expired): ask again.
				b, err = c.Store.EnsureAddonBranch(ctx, a, d.Branch)
			}
			if err != nil {
				return err
			}
			key := a.Name + "/branch"
			switch b.Status {
			case store.BranchFailed:
				return fmt.Errorf("veritabanı kopyası başarısız (%s → %s): %s", a.Name, b.Database, b.Error)
			case store.BranchReady:
				if said[key] != "ready" {
					say(key, "ready", "==> veritabanı kopyası: %s hazır (%s%s%s)", a.Name, sizeNote(b), snapshotNote(b), rulesNote(a, b))
					if b.Warning != "" {
						log("==> veritabanı kopyası: UYARI: %s", WarningTR(b.Warning))
					}
				}
			case store.BranchDeleting:
				waiting = true
				say(key, "deleting", "==> veritabanı kopyası: %s için eski kopya siliniyor, sonra yeniden oluşturulacak", a.Name)
			case store.BranchCopying:
				waiting = true
				say(key, "copying", "==> veritabanı kopyası: %s → %s kopyalanıyor…", a.Name, b.Database)
			default:
				waiting = true
				say(key, "pending", "==> veritabanı kopyası: %s → %s sırada", a.Name, b.Database)
			}
		}
		if !waiting {
			return nil
		}
		c.Kick(d.AppID)
		select {
		case <-ctx.Done():
			return fmt.Errorf("waited %s for the databases: %w", c.Now().Sub(start).Round(time.Second), ctx.Err())
		case <-time.After(c.WaitPoll):
		}
	}
}

func modeNote(mode string, a store.Addon) string {
	if mode == store.PreviewEmpty {
		return "boş veritabanı"
	}
	if n := len(a.Anonymize); n > 0 || a.AnonymizeSQL != "" {
		return "production'dan kopyalanıyor, anonimleştirilecek"
	}
	return "production'dan kopyalanıyor"
}

func sizeNote(b store.AddonBranch) string {
	if b.SizeBytes == nil {
		return b.Mode
	}
	return HumanBytes(*b.SizeBytes)
}

func snapshotNote(b store.AddonBranch) string {
	if b.SnapshotAt == nil || b.Mode != store.PreviewCopy {
		return ""
	}
	return ", anlık görüntü " + b.SnapshotAt.UTC().Format("2006-01-02 15:04:05") + " UTC"
}

func rulesNote(a store.Addon, b store.AddonBranch) string {
	if b.Mode != store.PreviewCopy {
		return ", boş veritabanı"
	}
	out := ""
	if n := len(a.Anonymize); n > 0 {
		out += fmt.Sprintf(", %d anonimleştirme kuralı", n)
	}
	if a.AnonymizeSQL != "" {
		out += ", anonimleştirme SQL'i"
	}
	return out
}
