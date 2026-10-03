// Command minipaas runs the control plane: HTTP API, GitHub webhook receiver
// and deployment worker in one process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nisagwn/minipaas/internal/api"
	"github.com/nisagwn/minipaas/internal/build"
	"github.com/nisagwn/minipaas/internal/config"
	"github.com/nisagwn/minipaas/internal/deploy"
	"github.com/nisagwn/minipaas/internal/routing"
	"github.com/nisagwn/minipaas/internal/store"
	"github.com/nisagwn/minipaas/internal/worker"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	pipeline, applier, err := newPipeline(cfg, st)
	if err != nil {
		return err
	}
	// Alias routing exists only with a real deployer; nil interfaces otherwise.
	var router *routing.Syncer
	var apiRouter api.Router
	var workerRouter worker.Router
	if applier != nil {
		router = &routing.Syncer{Store: st, Applier: applier, Interval: cfg.RouteSyncInterval, Log: log}
		apiRouter, workerRouter = router, router
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: (&api.Server{
			Store: st, Router: apiRouter, Domain: cfg.Domain, APIToken: cfg.APIToken,
			WebhookSecret: cfg.GitHubWebhookSecret, Log: log,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	w := &worker.Worker{
		Store: st, Pipeline: pipeline, Router: workerRouter, Domain: cfg.Domain,
		PollInterval: cfg.PollInterval, Concurrency: cfg.Workers, Timeout: cfg.DeployTimeout, Log: log,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Run(ctx)
	}()
	if router != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			router.Run(ctx)
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "domain", cfg.Domain,
			"builder", cfg.Builder, "deployer", cfg.Deployer, "registry", cfg.Registry)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		stop()
		return err
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	wg.Wait() // let in-flight deployments finish
	return nil
}

// newPipeline also returns the alias applier when the deployer has one.
func newPipeline(cfg config.Config, st *store.Store) (worker.Pipeline, routing.Applier, error) {
	dry := worker.DryRunPipeline{Registry: "registry.local", Step: 500 * time.Millisecond}
	var p worker.Stages
	var applier routing.Applier

	switch cfg.Builder {
	case "dryrun":
		p.Builder = dry
	case "buildkit", "docker":
		var engine build.Engine = build.Docker{}
		if cfg.Builder == "buildkit" {
			engine = build.BuildKit{Addr: cfg.BuildKitAddr, Insecure: cfg.RegistryInsecure}
		}
		b := &build.Builder{
			Engine: engine, Registry: cfg.Registry, Platform: cfg.BuildPlatform, Cache: cfg.BuildCache,
			GitBaseURL: cfg.GitBaseURL, GitToken: cfg.GitHubToken,
		}
		if err := b.Check(); err != nil {
			return nil, nil, err
		}
		p.Builder = b
	default:
		return nil, nil, fmt.Errorf("unknown MINIPAAS_BUILDER %q (dryrun, buildkit, docker)", cfg.Builder)
	}

	switch cfg.Deployer {
	case "dryrun":
		p.Deployer = dry
	case "kubernetes":
		client, err := deploy.NewClient(cfg.Kubeconfig)
		if err != nil {
			return nil, nil, err
		}
		d, err := deploy.New(client, st, deploy.Config{
			Domain: cfg.Domain, IngressClass: cfg.IngressClass, TLS: cfg.IngressTLS,
			CPURequest: cfg.AppCPURequest, CPULimit: cfg.AppCPULimit,
			MemoryRequest: cfg.AppMemoryRequest, MemoryLimit: cfg.AppMemoryLimit,
			QuotaCPU: cfg.AppQuotaCPU, QuotaMemory: cfg.AppQuotaMemory, QuotaPods: cfg.AppQuotaPods,
			RunAsNonRoot: cfg.AppRunAsNonRoot, RolloutTimeout: cfg.RolloutTimeout,
		})
		if err != nil {
			return nil, nil, err
		}
		if err := d.Check(); err != nil {
			return nil, nil, err
		}
		p.Deployer = d
		applier = d
	default:
		return nil, nil, fmt.Errorf("unknown MINIPAAS_DEPLOYER %q (dryrun, kubernetes)", cfg.Deployer)
	}
	return p, applier, nil
}
