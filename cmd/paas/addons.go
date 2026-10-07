package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/nisagwn/paas/internal/addons"
	"github.com/nisagwn/paas/internal/config"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/routing"
	"github.com/nisagwn/paas/internal/store"
)

// startAddons runs the add-on reconcile loop (Faz 22) and returns it; the
// worker waits for databases through it. It runs on every replica; add-ons
// are claimed with FOR UPDATE SKIP LOCKED. Without the Kubernetes deployer
// it runs in dry-run mode (add-ons become ready without a server).
func startAddons(ctx context.Context, st *store.Store, applier routing.Applier, log *slog.Logger, wg *sync.WaitGroup) (*addons.Controller, error) {
	cfg, err := config.LoadAddons()
	if err != nil {
		return nil, err
	}
	var cluster addons.Cluster
	if k, ok := applier.(*deploy.Kubernetes); ok && k != nil {
		cluster = k
	}
	c := addons.New(st, cluster, log)
	c.Interval, c.CopyTimeout, c.CopyMaxBytes = cfg.Interval, cfg.CopyTimeout, cfg.CopyMaxBytes
	log.Info("add-on reconcile loop", "interval", cfg.Interval, "cluster", cluster != nil,
		"copy_max_bytes", cfg.CopyMaxBytes, "storage_class", cfg.StorageClass)
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Run(ctx)
	}()
	return c, nil
}
