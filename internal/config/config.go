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
	// Bearer token required for /api/* endpoints.
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
	}
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

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "PAAS_DATABASE_URL")
	}
	if c.APIToken == "" {
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
