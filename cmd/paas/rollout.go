package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/nisagwn/paas/internal/config"
	"github.com/nisagwn/paas/internal/rollout"
	"github.com/nisagwn/paas/internal/routing"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

// startRollouts runs the canary/guard controller (Faz 21). It runs on every
// replica; rollouts are claimed with FOR UPDATE SKIP LOCKED. Without a
// router (dry-run deployer) weights are only recorded. notifier is the
// worker's (GitHub commit statuses) when it can report rollouts.
func startRollouts(ctx context.Context, st *store.Store, router *routing.Syncer, notifier worker.Notifier,
	log *slog.Logger, wg *sync.WaitGroup) error {
	interval, err := config.RolloutInterval()
	if err != nil {
		return err
	}
	c := &rollout.Controller{Store: st, Interval: interval, Log: log}
	if router != nil {
		c.Router = router
	}
	if n, ok := notifier.(rollout.Notifier); ok {
		c.Notifier = n
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Run(ctx)
	}()
	return nil
}
