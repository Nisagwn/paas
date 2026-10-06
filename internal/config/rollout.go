package config

import (
	"fmt"
	"os"
	"time"
)

// RolloutInterval is how often the rollout controller (Faz 21,
// internal/rollout) evaluates running canary and guarded rollouts:
// PAAS_ROLLOUT_INTERVAL, default 30s, at least 5s. The per-app settings
// (mode, steps, thresholds) live in the database.
func RolloutInterval() (time.Duration, error) {
	v := os.Getenv("PAAS_ROLLOUT_INTERVAL")
	if v == "" {
		return 30 * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 5*time.Second {
		return 0, fmt.Errorf("PAAS_ROLLOUT_INTERVAL must be a duration of at least 5s (e.g. 30s)")
	}
	return d, nil
}
