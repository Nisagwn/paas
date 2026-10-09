package main

import (
	"context"
	"log/slog"

	"github.com/nisagwn/paas/internal/config"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/routing"
	"github.com/nisagwn/paas/internal/scale"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/terminal"
)

// newTerminal builds the browser terminal (Faz 27). It returns nil (the UI
// then has no terminal at all) when PAAS_TERMINAL=false or the deployer is
// not Kubernetes.
func newTerminal(cfg config.Config, st *store.Store, applier routing.Applier, sc *scale.Scaler, log *slog.Logger) (*terminal.Service, error) {
	if !cfg.Terminal.Enabled {
		log.Info("browser terminal disabled (PAAS_TERMINAL=false)")
		return nil, nil
	}
	if _, ok := applier.(*deploy.Kubernetes); !ok {
		return nil, nil
	}
	cluster, err := deploy.NewTerminal(cfg.Kubeconfig)
	if err != nil {
		return nil, err
	}
	svc := &terminal.Service{
		Cluster: cluster, Audit: st, Log: log,
		Config: terminal.Config{
			IdleTimeout: cfg.Terminal.IdleTimeout, MaxDuration: cfg.Terminal.MaxDuration, MaxPerUser: cfg.Terminal.MaxPerUser,
		},
	}
	if sc != nil {
		svc.Waker = scalerWaker{sc}
	}
	log.Info("browser terminal enabled", "idle_timeout", svc.IdleTimeout(), "max_duration", svc.MaxDuration(),
		"max_sessions", svc.MaxPerUser(), "wake", sc != nil)
	return svc, nil
}

// scalerWaker wakes sleeping deployments through the Faz 11 scaler, which
// also records the activity so the idle check does not put them back to
// sleep while a session is open.
type scalerWaker struct{ sc *scale.Scaler }

func workload(d store.Deployment) deploy.Workload {
	return deploy.Workload{Namespace: naming.Namespace(d.AppName), Name: d.ObjectName(), DeploymentID: d.ID,
		Sleeping: d.SleepingSince != nil}
}

func (w scalerWaker) Wake(ctx context.Context, d store.Deployment) error {
	return w.sc.Wake(ctx, workload(d))
}
func (w scalerWaker) KeepAwake(d store.Deployment) { w.sc.KeepAwake(workload(d)) }
