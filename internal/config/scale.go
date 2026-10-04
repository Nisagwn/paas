package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// Scale configures scale to zero (Faz 11, internal/scale).
type Scale struct {
	// Deployments without requests this long sleep; 0 disables. Default 30m.
	After time.Duration
	// Interval of the idle check; 0 picks min(After/4, 30s).
	Interval time.Duration
	// Listen address of the activator, e.g. ":8081".
	ActivatorAddr string
	// ActivatorPort is the port of ActivatorAddr.
	ActivatorPort int32
	// IP the ingress controller dials to reach the activator: the control
	// plane pod's IP in the cluster (downward API), the host's address as
	// seen from the cluster otherwise (k3d: host.k3d.internal).
	ActivatorIP string
	// "pod" (default) proxies woken requests to the pod; an http(s) URL
	// re-sends them through the ingress controller instead.
	ActivatorUpstream string
	// Namespace of the control plane; when set, app NetworkPolicies let it
	// reach app pods (needed by the "pod" upstream).
	ControlPlaneNamespace string
	// Where Traefik's metrics are read (deploy.Config).
	TraefikNamespace, TraefikMetricsPort string
	// Disabled says why scale to zero is off despite After > 0 (logged).
	Disabled string
}

// Enabled reports whether idle deployments are scaled to zero.
func (s Scale) Enabled() bool { return s.After > 0 }

func (c *Config) loadScale() error {
	s := Scale{
		After:                 30 * time.Minute,
		ActivatorAddr:         getenv("PAAS_ACTIVATOR_ADDR", ":8081"),
		ActivatorIP:           os.Getenv("PAAS_ACTIVATOR_IP"),
		ActivatorUpstream:     getenv("PAAS_ACTIVATOR_UPSTREAM", "pod"),
		ControlPlaneNamespace: os.Getenv("PAAS_CONTROL_PLANE_NAMESPACE"),
		TraefikNamespace:      os.Getenv("PAAS_TRAEFIK_NAMESPACE"),
		TraefikMetricsPort:    os.Getenv("PAAS_TRAEFIK_METRICS_PORT"),
	}
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{{"PAAS_SCALE_TO_ZERO_AFTER", &s.After}, {"PAAS_SCALE_INTERVAL", &s.Interval}} {
		if v := os.Getenv(d.key); v != "" {
			n, err := time.ParseDuration(v)
			if err != nil || n < 0 {
				return fmt.Errorf("%s must be a duration (e.g. 30m; 0 disables)", d.key)
			}
			*d.dst = n
		}
	}
	if s.Enabled() && c.Deployer == "kubernetes" {
		_, port, err := net.SplitHostPort(s.ActivatorAddr)
		p, perr := strconv.Atoi(port)
		if err != nil || perr != nil || p < 1 || p > 65535 {
			return errors.New("PAAS_ACTIVATOR_ADDR must be host:port, e.g. :8081")
		}
		s.ActivatorPort = int32(p)
		switch {
		case s.ActivatorIP == "":
			// Nowhere to send sleeping deployments' traffic: stay awake.
			s.After = 0
			s.Disabled = "PAAS_ACTIVATOR_IP is not set"
		case net.ParseIP(s.ActivatorIP) == nil:
			return errors.New("PAAS_ACTIVATOR_IP must be the IP the ingress controller reaches the activator at")
		}
	}
	c.Scale = s
	return nil
}
