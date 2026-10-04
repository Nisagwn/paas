package config

import (
	"fmt"
	"os"
	"time"
)

// DomainConfig holds the custom domain settings (Faz 12).
type DomainConfig struct {
	// How often pending, verified and failing domains are checked.
	CheckInterval time.Duration
	// How often active domains are re-validated.
	RecheckInterval time.Duration
	// A verified domain whose DNS check fails keeps serving this long.
	Grace time.Duration
	// "dns" (default) or "skip": route without DNS checks (development only).
	Verify string
	// cert-manager ClusterIssuer for per-domain certificates (HTTP-01).
	Issuer string
}

func (c *Config) loadDomains() error {
	c.Domains = DomainConfig{
		CheckInterval: time.Minute, RecheckInterval: time.Hour, Grace: 72 * time.Hour,
		Verify: getenv("PAAS_DOMAIN_VERIFY", "dns"),
		Issuer: getenv("PAAS_CUSTOM_DOMAIN_ISSUER", "letsencrypt-http01"),
	}
	if c.Domains.Verify != "dns" && c.Domains.Verify != "skip" {
		return fmt.Errorf("PAAS_DOMAIN_VERIFY must be dns or skip, got %q", c.Domains.Verify)
	}
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{
		{"PAAS_DOMAIN_CHECK_INTERVAL", &c.Domains.CheckInterval},
		{"PAAS_DOMAIN_RECHECK_INTERVAL", &c.Domains.RecheckInterval},
		{"PAAS_DOMAIN_GRACE", &c.Domains.Grace},
	} {
		if v := os.Getenv(d.key); v != "" {
			n, err := time.ParseDuration(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("%s must be a positive duration (e.g. 1m)", d.key)
			}
			*d.dst = n
		}
	}
	return nil
}
