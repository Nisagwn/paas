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
	"github.com/nisagwn/paas/internal/webhook"
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
	if n, err := loadDotEnv(".env"); err != nil {
		return err
	} else if n > 0 {
		log.Info("loaded .env", "vars", n)
	}
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

	// Faz 15: the GitHub App, when configured, authenticates clones,
	// statuses and comments with installation tokens; repositories it is
	// not installed on keep using PAAS_GITHUB_TOKEN (if set).
	ghApp, err := newGitHubApp(cfg, st, log)
	if err != nil {
		return err
	}
	var repoTokens github.TokenSource
	if ghApp != nil {
		repoTokens = github.AppTokens{App: ghApp, Fallback: cfg.GitHubToken}
	}

	pipeline, applier, err := newPipeline(cfg, st, repoTokens)
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
	runtimeLogs, _ := applier.(api.RuntimeLogs) // only the Kubernetes deployer
	var _ api.RuntimeLogs = (*deploy.Kubernetes)(nil)

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
	// Faz 13: users, teams and GitHub login.
	sessions, authn, ghLogin := newAuth(cfg, st, log)
	ui := &web.Server{
		Store: st, Router: apiRouter, Sessions: sessions, Domain: cfg.Domain, Scheme: scheme,
		RuntimeLogs: runtimeLogs != nil, Log: log, Domains: verifier, Auth: authn, GitHub: ghLogin,
	}
	// Faz 15: install link and import. Assigned only with an App, so the
	// interfaces stay nil without one.
	var inspector api.RepoInspector
	if ghApp != nil {
		inspector = ghApp
		ui.GitHubAppSlug, ui.GitHubApp = cfg.GitHubApp.Slug, inspector
	}

	// Long-lived streams (SSE) end when shutdown starts instead of holding it up.
	reqCtx, cancelReqs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelReqs()
	apiSrv := &api.Server{
		Store: st, Router: apiRouter, Domain: cfg.Domain, Scheme: scheme, APIToken: cfg.APIToken,
		WebhookSecret: cfg.GitHubWebhookSecret, Log: log, Cleanup: gc,
		Sessions: sessions, Events: hub, RuntimeLogs: runtimeLogs, UI: ui.Handler(), Domains: verifier,
		Auth: authn, GitHubApp: inspector,
	}
	handler := apiSrv.Handler()
	if ghApp != nil {
		// The App's installation events share /webhooks/github with push
		// and pull_request; everything else reaches the API unchanged.
		handler = &webhook.Installations{Secret: cfg.GitHubWebhookSecret, Store: st, Log: log, Next: handler}
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
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
	if (cfg.GitHubToken != "" || ghApp != nil) && cfg.GitHubStatus {
		client := github.New(cfg.GitHubAPIURL, cfg.GitHubToken, log)
		client.Tokens = repoTokens // nil without the App: the token as before
		w.Notifier = &github.Notifier{
			Client: client, PublicURL: cfg.PublicURL, Context: cfg.GitHubStatusContext, Log: log,
		}
		log.Info("github commit statuses and PR comments enabled", "api", cfg.GitHubAPIURL,
			"public_url", cfg.PublicURL, "app", ghApp != nil)
	}
	w.Cleanup, w.StaleAfter = gc, cfg.WorkerStaleAfter

	var wg sync.WaitGroup
	// Faz 19: request analytics and live resource usage (read per request).
	apiSrv.Usage = startAnalytics(ctx, cfg, st, applier, log, &wg)
	ui.Usage = apiSrv.Usage // Faz 16–19 screens: the Analitik tab; read per request, set before serving
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
	if ghApp != nil {
		syncer := &github.InstallationSyncer{App: ghApp, Store: st, Log: log}
		wg.Add(1)
		go func() {
			defer wg.Done()
			syncer.Run(ctx)
		}()
	}

	// Faz 11: scale idle deployments to zero; the activator wakes them.
	errCh := make(chan error, 2)
	activatorSrv, err := startScaler(ctx, cfg, st, applier, log, &wg, errCh)
	if err != nil {
		return err
	}
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
	if activatorSrv != nil {
		activatorSrv.Shutdown(shutdownCtx)
	}
	wg.Wait() // let in-flight deployments finish
	return nil
}

// newAuth wires sessions, the authenticator and, when an OAuth App is
// configured, GitHub login. Without one the UI offers the token login.
func newAuth(cfg config.Config, st *store.Store, log *slog.Logger) (*auth.Sessions, *auth.Authenticator, *auth.GitHubLogin) {
	sessions := auth.NewWithKey(cfg.SessionKey, cfg.APIToken)
	// New members are resolved through GitHub's public API (with
	// PAAS_GITHUB_TOKEN when set, for a higher rate limit).
	authn := &auth.Authenticator{
		Store: st, Sessions: sessions, OAuth: cfg.OAuthEnabled(), Log: log,
		Users: auth.GitHubDirectory{Client: github.New(cfg.GitHubAPIURL, cfg.GitHubToken, log)},
	}
	if !cfg.OAuthEnabled() {
		log.Warn("GitHub login not configured: the web UI accepts the API token (development mode)")
		return sessions, authn, nil
	}
	gl := &auth.GitHubLogin{
		App: &github.OAuthApp{
			ClientID: cfg.GitHubOAuthClientID, ClientSecret: cfg.GitHubOAuthClientSecret, WebURL: cfg.GitHubWebURL,
		},
		APIURL: cfg.GitHubAPIURL, RedirectURI: cfg.PublicURL + "/auth/github/callback",
		Admins: cfg.AdminGitHubLogins, AllowedUsers: cfg.AllowedGitHubUsers, AllowedOrgs: cfg.AllowedGitHubOrgs, OpenSignup: cfg.OpenSignup, Log: log,
	}
	log.Info("github login enabled", "callback", gl.RedirectURI, "admins", len(gl.Admins),
		"allowed_users", len(gl.AllowedUsers), "allowed_orgs", gl.AllowedOrgs, "open_signup", gl.OpenSignup, "legacy_token", cfg.APIToken != "")
	if len(gl.Admins)+len(gl.AllowedUsers)+len(gl.AllowedOrgs) == 0 && !gl.OpenSignup {
		log.Warn("no GitHub allowlist: only users already added to a team can sign in " +
			"(set PAAS_ADMIN_GITHUB_LOGINS to bootstrap the first owner)")
	}
	return sessions, authn, gl
}

// newGitHubApp returns the GitHub App, or nil when it is not configured.
// A key that does not parse stops startup.
func newGitHubApp(cfg config.Config, st *store.Store, log *slog.Logger) (*github.App, error) {
	if !cfg.GitHubApp.Enabled() {
		return nil, nil
	}
	key, err := github.ParsePrivateKey(cfg.GitHubApp.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("PAAS_GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	app := github.NewApp(cfg.GitHubAPIURL, cfg.GitHubApp.ID, key, log)
	app.Installations = st
	log.Info("github app enabled", "app_id", cfg.GitHubApp.ID, "slug", cfg.GitHubApp.Slug,
		"token_fallback", cfg.GitHubToken != "")
	return app, nil
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
// repoTokens (the GitHub App) replaces PAAS_GITHUB_TOKEN for clones when set.
func newPipeline(cfg config.Config, st *store.Store, repoTokens github.TokenSource) (worker.Pipeline, routing.Applier, error) {
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
		if repoTokens != nil {
			b.GitTokens = repoTokens
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
			CustomDomainIssuer: cfg.Domains.Issuer, CertIssuer: cfg.IngressCertIssuer,
			ActivatorIP: cfg.Scale.ActivatorIP, ActivatorPort: cfg.Scale.ActivatorPort,
			ActivatorNamespace: activatorNamespace(cfg.Scale),
			TraefikNamespace:   cfg.Scale.TraefikNamespace, TraefikMetricsPort: cfg.Scale.TraefikMetricsPort,
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
