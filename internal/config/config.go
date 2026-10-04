// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	// HTTP listen address, e.g. ":8080".
	Addr string
	// PostgreSQL connection string.
	DatabaseURL string
	// Base domain for generated hostnames, e.g. "paas.example.com".
	Domain string
	// Legacy admin/break-glass bearer token for /api/* (Faz 13: optional
	// when GitHub login is configured; empty disables it).
	APIToken string
	// Shared secret configured on the GitHub webhook.
	GitHubWebhookSecret string
	// Build stage: "dryrun", "buildkit" (buildctl → buildkitd, used in the
	// cluster) or "docker" (docker buildx, for local development).
	Builder string
	// Deploy stage: "dryrun" or "kubernetes".
	Deployer string
	// Image registry prefix, e.g. "localhost:5000" or "ghcr.io/nisagwn".
	Registry string
	// Push to a plain-HTTP registry (local development only).
	RegistryInsecure bool
	// buildkitd address for the "buildkit" builder.
	BuildKitAddr string
	// Target platform, e.g. "linux/arm64". Empty builds for the builder's own.
	BuildPlatform string
	// Export/import a registry layer cache per app.
	BuildCache bool
	// Base URL that "owner/repo" is appended to for cloning.
	GitBaseURL string
	// Token for cloning private GitHub repositories (optional).
	GitHubToken string
	// kubeconfig for the "kubernetes" deployer. Empty: in-cluster config,
	// else $KUBECONFIG / ~/.kube/config.
	Kubeconfig string
	// Bounds the wait for a new Deployment to become available.
	RolloutTimeout time.Duration
	// Per-container resources and per-app ResourceQuota (Kubernetes quantities).
	AppCPURequest, AppCPULimit, AppMemoryRequest, AppMemoryLimit string
	AppQuotaCPU, AppQuotaMemory                                  string
	AppQuotaPods                                                 int
	// Set runAsNonRoot on app pods (default true). Generated Dockerfiles use
	// numeric USERs; a repo Dockerfile with a named USER (e.g. "USER node")
	// is rejected by the kubelet because it cannot verify the UID.
	AppRunAsNonRoot bool
	// Ingress class for app routes ("traefik" on k3s; empty = cluster default).
	IngressClass string
	// Serve app routes over HTTPS with the default wildcard certificate.
	IngressTLS bool
	// How often alias routes are reconciled with the database.
	RouteSyncInterval time.Duration
	// Bounds a whole deployment (build + deploy).
	DeployTimeout time.Duration
	// How often the worker polls for queued deployments.
	PollInterval time.Duration
	// Number of deployments processed concurrently.
	Workers int
	// Where the control plane is reachable; links in GitHub statuses and PR
	// comments point here. Default "https://" + Domain.
	PublicURL string
	// GitHub REST API base (GitHub Enterprise: https://<host>/api/v3).
	GitHubAPIURL string
	// Report commit statuses and PR comments (needs GitHubToken).
	GitHubStatus bool
	// Commit status context shown on GitHub.
	GitHubStatusContext string

	// Faz 7: retention and recovery (see internal/cleanup).
	// Ready production deployments kept as rollback targets.
	KeepProduction int
	// Unaliased preview deployments are retired this long after finishing.
	PreviewTTL time.Duration
	// Preview aliases idle this long are removed (0 = never).
	PreviewAliasTTL time.Duration
	// Interval of the full cleanup sweep.
	GCInterval time.Duration
	// A deployment without a worker heartbeat this long is recovered.
	WorkerStaleAfter time.Duration

	// Faz 12: custom domains (domains.go).
	Domains DomainConfig
	// Faz 13: users and teams (see loadUsers).
	// Signs session cookies; empty derives a key from APIToken.
	SessionKey string
	// GitHub OAuth App for the web login; both empty = dev mode (token login).
	GitHubOAuthClientID, GitHubOAuthClientSecret string
	// Where users authorize the OAuth App (GitHub Enterprise: https://<host>).
	GitHubWebURL string
	// Who may sign in, besides users invited to a team. Logins and orgs are
	// compared case-insensitively.
	AllowedGitHubUsers []string
	AllowedGitHubOrgs  []string
	// Always allowed; made owners of the "default" team at sign-in.
	AdminGitHubLogins []string
	// Faz 11: scale to zero (see scale.go).
	Scale Scale
}

// OAuthEnabled reports whether GitHub login is configured.
func (c Config) OAuthEnabled() bool { return c.GitHubOAuthClientID != "" }

// loadUsers reads the Faz 13 settings. PAAS_API_TOKEN is required in dev
// mode (it is the only way in); with GitHub login it is optional, and the
// session key must then come from PAAS_SESSION_KEY or the token.
func (c *Config) loadUsers() error {
	c.SessionKey = os.Getenv("PAAS_SESSION_KEY")
	c.GitHubOAuthClientID = os.Getenv("PAAS_GITHUB_OAUTH_CLIENT_ID")
	c.GitHubOAuthClientSecret = os.Getenv("PAAS_GITHUB_OAUTH_CLIENT_SECRET")
	c.GitHubWebURL = strings.TrimRight(getenv("PAAS_GITHUB_WEB_URL", "https://github.com"), "/")
	c.AllowedGitHubUsers = splitList(os.Getenv("PAAS_ALLOWED_GITHUB_USERS"))
	c.AllowedGitHubOrgs = splitList(os.Getenv("PAAS_ALLOWED_GITHUB_ORG"))
	c.AdminGitHubLogins = splitList(os.Getenv("PAAS_ADMIN_GITHUB_LOGINS"))
	if (c.GitHubOAuthClientID == "") != (c.GitHubOAuthClientSecret == "") {
		return errors.New("set both PAAS_GITHUB_OAUTH_CLIENT_ID and PAAS_GITHUB_OAUTH_CLIENT_SECRET, or neither")
	}
	if c.SessionKey != "" && len(c.SessionKey) < 32 {
		return errors.New("PAAS_SESSION_KEY must be at least 32 characters")
	}
	if c.OAuthEnabled() && c.APIToken == "" && c.SessionKey == "" {
		return errors.New("GitHub login without PAAS_API_TOKEN needs PAAS_SESSION_KEY")
	}
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, strings.TrimPrefix(f, "@"))
	}
	return out
}

// loadLifecycle reads the Faz 7 settings.
func (c *Config) loadLifecycle() error {
	c.KeepProduction, c.PreviewTTL, c.GCInterval, c.WorkerStaleAfter = 5, 72*time.Hour, 10*time.Minute, 2*time.Minute
	if v := os.Getenv("PAAS_KEEP_PRODUCTION"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &c.KeepProduction); err != nil || c.KeepProduction < 0 {
			return errors.New("PAAS_KEEP_PRODUCTION must be a non-negative integer")
		}
	}
	for _, d := range []struct {
		key      string
		dst      *time.Duration
		positive bool
	}{
		{"PAAS_PREVIEW_TTL", &c.PreviewTTL, false},
		{"PAAS_PREVIEW_ALIAS_TTL", &c.PreviewAliasTTL, false},
		{"PAAS_GC_INTERVAL", &c.GCInterval, true},
		{"PAAS_WORKER_STALE_AFTER", &c.WorkerStaleAfter, true},
	} {
		v := os.Getenv(d.key)
		if v == "" {
			continue
		}
		n, err := time.ParseDuration(v)
		if err != nil || n < 0 || (d.positive && n == 0) {
			return fmt.Errorf("%s must be a duration (e.g. 72h)", d.key)
		}
		*d.dst = n
	}
	return nil
}

func Load() (Config, error) {
	c := Config{
		Addr:                getenv("PAAS_ADDR", ":8080"),
		DatabaseURL:         os.Getenv("PAAS_DATABASE_URL"),
		Domain:              strings.ToLower(getenv("PAAS_DOMAIN", "localtest.me")),
		APIToken:            os.Getenv("PAAS_API_TOKEN"),
		GitHubWebhookSecret: os.Getenv("PAAS_GITHUB_WEBHOOK_SECRET"),
		Builder:             getenv("PAAS_BUILDER", "dryrun"),
		Deployer:            getenv("PAAS_DEPLOYER", "dryrun"),
		Registry:            os.Getenv("PAAS_REGISTRY"),
		RegistryInsecure:    os.Getenv("PAAS_REGISTRY_INSECURE") == "true",
		BuildKitAddr:        getenv("PAAS_BUILDKIT_ADDR", "tcp://127.0.0.1:1234"),
		BuildPlatform:       os.Getenv("PAAS_BUILD_PLATFORM"),
		BuildCache:          os.Getenv("PAAS_BUILD_CACHE") == "true",
		GitBaseURL:          getenv("PAAS_GIT_BASE_URL", "https://github.com"),
		GitHubToken:         os.Getenv("PAAS_GITHUB_TOKEN"),
		Kubeconfig:          os.Getenv("PAAS_KUBECONFIG"),
		AppCPURequest:       getenv("PAAS_APP_CPU_REQUEST", "25m"),
		AppCPULimit:         getenv("PAAS_APP_CPU_LIMIT", "500m"),
		AppMemoryRequest:    getenv("PAAS_APP_MEMORY_REQUEST", "64Mi"),
		AppMemoryLimit:      getenv("PAAS_APP_MEMORY_LIMIT", "256Mi"),
		AppQuotaCPU:         getenv("PAAS_APP_QUOTA_CPU", "1"),
		AppQuotaMemory:      getenv("PAAS_APP_QUOTA_MEMORY", "4Gi"),
		AppQuotaPods:        20,
		AppRunAsNonRoot:     os.Getenv("PAAS_APP_RUN_AS_NON_ROOT") != "false",
		RolloutTimeout:      3 * time.Minute,
		IngressClass:        getenv("PAAS_INGRESS_CLASS", "traefik"),
		IngressTLS:          os.Getenv("PAAS_INGRESS_TLS") != "false",
		RouteSyncInterval:   time.Minute,
		PollInterval:        2 * time.Second,
		Workers:             2,
		DeployTimeout:       15 * time.Minute,
		GitHubAPIURL:        getenv("PAAS_GITHUB_API_URL", "https://api.github.com"),
		GitHubStatus:        os.Getenv("PAAS_GITHUB_STATUS") != "false",
		GitHubStatusContext: getenv("PAAS_GITHUB_STATUS_CONTEXT", "paas/deploy"),
	}
	c.PublicURL = strings.TrimRight(getenv("PAAS_PUBLIC_URL", "https://"+c.Domain), "/")
	if v := os.Getenv("PAAS_DEPLOY_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("PAAS_DEPLOY_TIMEOUT: %w", err)
		}
		c.DeployTimeout = d
	}
	if v := os.Getenv("PAAS_ROLLOUT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, errors.New("PAAS_ROLLOUT_TIMEOUT must be a positive duration")
		}
		c.RolloutTimeout = d
	}
	if v := os.Getenv("PAAS_ROUTE_SYNC_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, errors.New("PAAS_ROUTE_SYNC_INTERVAL must be a positive duration")
		}
		c.RouteSyncInterval = d
	}
	if v := os.Getenv("PAAS_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("PAAS_POLL_INTERVAL: %w", err)
		}
		c.PollInterval = d
	}
	if v := os.Getenv("PAAS_APP_QUOTA_PODS"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &c.AppQuotaPods); err != nil || c.AppQuotaPods < 1 {
			return c, errors.New("PAAS_APP_QUOTA_PODS must be a positive integer")
		}
	}
	if v := os.Getenv("PAAS_WORKERS"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &c.Workers); err != nil || c.Workers < 1 {
			return c, errors.New("PAAS_WORKERS must be a positive integer")
		}
	}

	if err := c.loadLifecycle(); err != nil {
		return c, err
	}
	if err := c.loadDomains(); err != nil {
		return c, err
	}
	if err := c.loadUsers(); err != nil {
		return c, err
	}
	if err := c.loadScale(); err != nil {
		return c, err
	}

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "PAAS_DATABASE_URL")
	}
	if c.APIToken == "" && !c.OAuthEnabled() {
		missing = append(missing, "PAAS_API_TOKEN")
	}
	if c.GitHubWebhookSecret == "" {
		missing = append(missing, "PAAS_GITHUB_WEBHOOK_SECRET")
	}
	if c.Builder != "dryrun" && c.Registry == "" {
		missing = append(missing, "PAAS_REGISTRY")
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
