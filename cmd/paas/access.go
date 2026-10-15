package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/nisagwn/paas/internal/routing"
)

// appAccess is implemented by the Kubernetes deployer (Faz 29); the dryrun
// pipeline has no cluster and skips it.
type appAccess interface {
	DetectServiceAccount(ctx context.Context) (string, error)
	ServiceAccount() string
	EnsureAllAppAccess(ctx context.Context) ([]string, error)
}

// ensureAppAccess finds out which ServiceAccount the control plane runs as
// and binds paas-app-manager to it in every existing app namespace, so
// namespaces created before least-privilege RBAC keep working. Failures
// are logged, not fatal: under the old cluster-wide ClusterRole nothing
// depends on the bindings, and every deployment ensures its own namespace's
// binding again.
func ensureAppAccess(applier routing.Applier, log *slog.Logger) {
	a, ok := applier.(appAccess)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	user, err := a.DetectServiceAccount(ctx)
	if err != nil {
		log.Warn("rbac: cannot tell the control plane's identity; app namespaces get no rolebinding", "err", err)
		return
	}
	if a.ServiceAccount() == "" {
		log.Info("rbac: not a service account; app namespaces get no rolebinding", "user", user)
		return
	}
	names, err := a.EnsureAllAppAccess(ctx)
	if err != nil {
		log.Warn("rbac: app namespace rolebindings", "service_account", a.ServiceAccount(), "err", err)
		return
	}
	log.Info("rbac: app namespace rolebindings ensured", "service_account", a.ServiceAccount(), "namespaces", len(names))
}
