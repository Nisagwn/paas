package routing

import (
	"context"
	"fmt"

	"github.com/nisagwn/paas/internal/store"
)

// Faz 24: deployment protection. Alias Ingresses get the protection
// middleware from ApplyAliases itself; ApplyProtection then brings the
// deployment Ingresses (written once, at deploy time) and the app's
// Middleware in line with the setting. It runs on every sync, so a
// settings change applies with the next SyncApp and drift is repaired by
// the reconcile loop.

// ProtectionApplier enforces an app's protection setting on its routes
// (deploy.Kubernetes). SyncApp uses it when the Applier implements it.
type ProtectionApplier interface {
	ApplyProtection(ctx context.Context, app string) error
}

func (s *Syncer) applyProtection(ctx context.Context, app store.App) error {
	pa, ok := s.Applier.(ProtectionApplier)
	if !ok {
		return nil
	}
	if err := pa.ApplyProtection(ctx, app.Name); err != nil {
		return fmt.Errorf("protection: %w", err)
	}
	return nil
}
