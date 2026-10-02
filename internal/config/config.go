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
	// Which pipeline the worker runs: "dryrun" (Faz 1) or "real" (Faz 2+).
	Pipeline string
	// How often the worker polls for queued deployments.
	PollInterval time.Duration
	// Number of deployments processed concurrently.
	Workers int
}

func Load() (Config, error) {
	c := Config{
		Addr:                getenv("MINIPAAS_ADDR", ":8080"),
		DatabaseURL:         os.Getenv("MINIPAAS_DATABASE_URL"),
		Domain:              strings.ToLower(getenv("MINIPAAS_DOMAIN", "localtest.me")),
		APIToken:            os.Getenv("MINIPAAS_API_TOKEN"),
		GitHubWebhookSecret: os.Getenv("MINIPAAS_GITHUB_WEBHOOK_SECRET"),
		Pipeline:            getenv("MINIPAAS_PIPELINE", "dryrun"),
		PollInterval:        2 * time.Second,
		Workers:             2,
	}
	if v := os.Getenv("MINIPAAS_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("MINIPAAS_POLL_INTERVAL: %w", err)
		}
		c.PollInterval = d
	}
	if v := os.Getenv("MINIPAAS_WORKERS"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &c.Workers); err != nil || c.Workers < 1 {
			return c, errors.New("MINIPAAS_WORKERS must be a positive integer")
		}
	}

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "MINIPAAS_DATABASE_URL")
	}
	if c.APIToken == "" {
		missing = append(missing, "MINIPAAS_API_TOKEN")
	}
	if c.GitHubWebhookSecret == "" {
		missing = append(missing, "MINIPAAS_GITHUB_WEBHOOK_SECRET")
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
