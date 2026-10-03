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

	pipeline, err := newPipeline(cfg)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: (&api.Server{
			Store: st, Domain: cfg.Domain, APIToken: cfg.APIToken,
			WebhookSecret: cfg.GitHubWebhookSecret, Log: log,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	w := &worker.Worker{
		Store: st, Pipeline: pipeline, Domain: cfg.Domain,
		PollInterval: cfg.PollInterval, Concurrency: cfg.Workers, Timeout: cfg.DeployTimeout, Log: log,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Run(ctx)
	}()

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

func newPipeline(cfg config.Config) (worker.Pipeline, error) {
	dry := worker.DryRunPipeline{Registry: "registry.local", Step: 500 * time.Millisecond}
	var p worker.Stages

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
			return nil, err
		}
		p.Builder = b
	default:
		return nil, fmt.Errorf("unknown MINIPAAS_BUILDER %q (dryrun, buildkit, docker)", cfg.Builder)
	}

	switch cfg.Deployer {
	case "dryrun":
		p.Deployer = dry
	default:
		return nil, fmt.Errorf("unknown MINIPAAS_DEPLOYER %q (only \"dryrun\" exists until Faz 3)", cfg.Deployer)
	}
	return p, nil
}
