package config

import (
	"fmt"
	"os"
	"time"
)

// Analytics configures request analytics (Faz 19, internal/analytics).
type Analytics struct {
	// Interval of the Traefik scrape; 0 disables collection. Default 1m.
	Interval time.Duration
	// Retention of the stored per-minute rows. Default 168h (7 days).
	Retention time.Duration
}

// Enabled reports whether request metrics are collected.
func (a Analytics) Enabled() bool { return a.Interval > 0 }

func (c *Config) loadAnalytics() error {
	a := Analytics{Interval: time.Minute, Retention: 7 * 24 * time.Hour}
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{{"PAAS_ANALYTICS_INTERVAL", &a.Interval}, {"PAAS_ANALYTICS_RETENTION", &a.Retention}} {
		if v := os.Getenv(d.key); v != "" {
			n, err := time.ParseDuration(v)
			if err != nil || n < 0 {
				return fmt.Errorf("%s must be a duration (e.g. 1m)", d.key)
			}
			*d.dst = n
		}
	}
	if a.Retention <= 0 {
		return fmt.Errorf("PAAS_ANALYTICS_RETENTION must be positive")
	}
	c.Analytics = a
	return nil
}
