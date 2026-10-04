// Command paas runs the control plane: HTTP API, GitHub webhook receiver
// and deployment worker in one process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/build"
	"github.com/nisagwn/paas/internal/cleanup"
	"github.com/nisagwn/paas/internal/config"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/domains"
	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/routing"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/web"
	"github.com/nisagwn/paas/internal/worker"
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
	if err := setupEnvEncryption(ctx, st, log); err != nil {
		return err
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
	// Faz 7: retention policy, failed leftovers, branch deletion.
	var gcRouter cleanup.Router
	if router != nil {
		gcRouter = router
	}
	gc := cleanup.New(st, retirerOf(pipeline), gcRouter, cleanup.Policy{
		KeepProduction: cfg.KeepProduction, PreviewTTL: cfg.PreviewTTL, PreviewAliasTTL: cfg.PreviewAliasTTL,
	}, cfg.GCInterval, log)

	// App URLs are HTTPS (wildcard certificate) unless Kubernetes routes are
	// served without TLS, e.g. on a laptop cluster.
	scheme := "https"
	if cfg.Deployer == "kubernetes" && !cfg.IngressTLS {
		scheme = "http"
	}

	// Faz 5: live logs, runtime logs and the web UI.
	hub := store.NewHub(cfg.DatabaseURL, log)
	sessions := auth.New(cfg.APIToken)
	runtimeLogs, _ := applier.(api.RuntimeLogs) // only the Kubernetes deployer

	// Faz 12: custom domain verification; routes follow production.
	verifier := &domains.Verifier{
		Store: st, Resolver: net.DefaultResolver, Domain: cfg.Domain, Mode: cfg.Domains.Verify,
		Interval: cfg.Domains.CheckInterval, Recheck: cfg.Domains.RecheckInterval, Grace: cfg.Domains.Grace, Log: log,
	}
	if router != nil {
		verifier.Router = router
	}
	if certs, ok := applier.(domains.Certificates); ok {
		verifier.Certs = certs
	}
	ui := &web.Server{
		Store: st, Router: apiRouter, Sessions: sessions, Domain: cfg.Domain, Scheme: scheme,
		RuntimeLogs: runtimeLogs != nil, Log: log, Domains: verifier,
	}

	// Long-lived streams (SSE) end when shutdown starts instead of holding it up.
	reqCtx, cancelReqs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelReqs()
	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: (&api.Server{
			Store: st, Router: apiRouter, Domain: cfg.Domain, Scheme: scheme, APIToken: cfg.APIToken,
			WebhookSecret: cfg.GitHubWebhookSecret, Log: log, Cleanup: gc,
			Sessions: sessions, Events: hub, RuntimeLogs: runtimeLogs, UI: ui.Handler(), Domains: verifier,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
	}
	srv.RegisterOnShutdown(cancelReqs)
	go func() {
		if err := hub.Run(ctx); err != nil {
			log.Error("log listener stopped; live logs fall back to periodic reads", "err", err)
		}
	}()
	w := &worker.Worker{
		Store: st, Pipeline: pipeline, Router: workerRouter, Domain: cfg.Domain, Scheme: scheme,
		PollInterval: cfg.PollInterval, Concurrency: cfg.Workers, Timeout: cfg.DeployTimeout, Log: log,
	}
	if cfg.GitHubToken != "" && cfg.GitHubStatus {
		w.Notifier = &github.Notifier{
			Client:    github.New(cfg.GitHubAPIURL, cfg.GitHubToken, log),
			PublicURL: cfg.PublicURL, Context: cfg.GitHubStatusContext, Log: log,
		}
		log.Info("github commit statuses and PR comments enabled", "api", cfg.GitHubAPIURL, "public_url", cfg.PublicURL)
	}
	w.Cleanup, w.StaleAfter = gc, cfg.WorkerStaleAfter

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
	wg.Add(1)
	go func() {
		defer wg.Done()
		gc.Run(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		verifier.Run(ctx)
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

// retirerOf returns the stage that deletes a deployment's objects.
func retirerOf(p worker.Pipeline) worker.Retirer {
	if s, ok := p.(worker.Stages); ok {
		r, _ := s.Deployer.(worker.Retirer)
		return r
	}
	r, _ := p.(worker.Retirer)
	return r
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
		return nil, nil, fmt.Errorf("unknown PAAS_BUILDER %q (dryrun, buildkit, docker)", cfg.Builder)
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
			CustomDomainIssuer: cfg.Domains.Issuer,
		})
		if err != nil {
			return nil, nil, err
		}
		if err := d.Check(); err != nil {
			return nil, nil, err
		}
		if d.Certificates, err = deploy.NewDynamicClient(cfg.Kubeconfig); err != nil {
			return nil, nil, err
		}
		p.Deployer = d
		applier = d
	default:
		return nil, nil, fmt.Errorf("unknown PAAS_DEPLOYER %q (dryrun, kubernetes)", cfg.Deployer)
	}
	return p, applier, nil
}
