package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Terminal configures the browser terminal (Faz 27, internal/terminal).
type Terminal struct {
	// Enabled: PAAS_TERMINAL (default true; "false" removes the terminal
	// entirely). It also needs the Kubernetes deployer.
	Enabled bool
	// IdleTimeout without input ends a session (PAAS_TERMINAL_IDLE_TIMEOUT, 10m).
	IdleTimeout time.Duration
	// MaxDuration ends any session (PAAS_TERMINAL_MAX_DURATION, 60m).
	MaxDuration time.Duration
	// MaxPerUser concurrent sessions (PAAS_TERMINAL_MAX_SESSIONS, 3).
	MaxPerUser int
}

func (c *Config) loadTerminal() error {
	t := Terminal{Enabled: os.Getenv("PAAS_TERMINAL") != "false",
		IdleTimeout: 10 * time.Minute, MaxDuration: 60 * time.Minute, MaxPerUser: 3}
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{{"PAAS_TERMINAL_IDLE_TIMEOUT", &t.IdleTimeout}, {"PAAS_TERMINAL_MAX_DURATION", &t.MaxDuration}} {
		if v := os.Getenv(d.key); v != "" {
			n, err := time.ParseDuration(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("%s must be a positive duration (e.g. 10m)", d.key)
			}
			*d.dst = n
		}
	}
	if v := os.Getenv("PAAS_TERMINAL_MAX_SESSIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("PAAS_TERMINAL_MAX_SESSIONS must be a positive integer")
		}
		t.MaxPerUser = n
	}
	c.Terminal = t
	return nil
}
