package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nisagwn/paas/internal/config"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/routing"
	"github.com/nisagwn/paas/internal/scale"
	"github.com/nisagwn/paas/internal/store"
)

// activatorNamespace opens app NetworkPolicies to the control plane only when
// the activator proxies straight to pods.
func activatorNamespace(s config.Scale) string {
	if s.Enabled() && (s.ActivatorUpstream == "" || s.ActivatorUpstream == "pod") {
		return s.ControlPlaneNamespace
	}
	return ""
}

// startScaler runs the idle check and the activator (Faz 11). It returns
// the activator's server, or nil when scale to zero is off.
func startScaler(ctx context.Context, cfg config.Config, st *store.Store, applier routing.Applier,
	log *slog.Logger, wg *sync.WaitGroup, errCh chan<- error) (*http.Server, error) {
	k, ok := applier.(*deploy.Kubernetes)
	if !ok || !cfg.Scale.Enabled() {
		if cfg.Scale.Disabled != "" {
			log.Warn("scale to zero disabled", "reason", cfg.Scale.Disabled)
		}
		return nil, nil
	}
	sc := &scale.Scaler{Cluster: k, Counter: k, Store: st, IdleAfter: cfg.Scale.After,
		Interval: cfg.Scale.Interval, Log: log}
	act := &scale.Activator{Scaler: sc, Cluster: k, Upstream: cfg.Scale.ActivatorUpstream,
		Timeout: cfg.RolloutTimeout + 30*time.Second, Log: log}
	if err := act.Init(); err != nil {
		return nil, err
	}
	srv := &http.Server{Addr: cfg.Scale.ActivatorAddr, Handler: act, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("activator listening", "addr", cfg.Scale.ActivatorAddr, "endpoint", cfg.Scale.ActivatorIP,
			"upstream", cfg.Scale.ActivatorUpstream, "idle_after", cfg.Scale.After)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc.Run(ctx)
	}()
	return srv, nil
}
