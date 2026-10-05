package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/nisagwn/paas/internal/analytics"
	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/config"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/routing"
	"github.com/nisagwn/paas/internal/store"
)

// startAnalytics runs the request metrics collector (Faz 19) and returns
// the live resource usage source for the API; both need the Kubernetes
// deployer, and the usage source is nil without it.
func startAnalytics(ctx context.Context, cfg config.Config, st *store.Store, applier routing.Applier,
	log *slog.Logger, wg *sync.WaitGroup) api.UsageSource {
	k, ok := applier.(*deploy.Kubernetes)
	if !ok {
		return nil
	}
	if cfg.Analytics.Enabled() {
		c := &analytics.Collector{Source: k, Store: st, Interval: cfg.Analytics.Interval,
			Retention: cfg.Analytics.Retention, Log: log}
		log.Info("request analytics enabled", "interval", cfg.Analytics.Interval, "retention", cfg.Analytics.Retention)
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Run(ctx)
		}()
	}
	return k
}
