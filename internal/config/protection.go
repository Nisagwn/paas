package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Protection configures deployment protection (Faz 24, internal/protect).
// The per-app settings (mode, scope, password, share links) live in the
// database.
type Protection struct {
	// ForwardAuthURL is the control plane's verify endpoint as Traefik
	// reaches it: PAAS_FORWARD_AUTH_URL, default
	// http://paas.paas.svc.cluster.local/_auth/verify (the in-cluster
	// Service). With the control plane on the host of a k3d cluster:
	// http://host.k3d.internal:<port>/_auth/verify.
	ForwardAuthURL string
}

// LoadProtection reads the protection settings from the environment.
func LoadProtection() (Protection, error) {
	p := Protection{ForwardAuthURL: getenv("PAAS_FORWARD_AUTH_URL", "http://paas.paas.svc.cluster.local/_auth/verify")}
	u, err := url.Parse(p.ForwardAuthURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return p, fmt.Errorf("PAAS_FORWARD_AUTH_URL must be an http(s) URL without query, e.g. http://paas.paas.svc.cluster.local/_auth/verify")
	}
	p.ForwardAuthURL = strings.TrimRight(p.ForwardAuthURL, "?")
	return p, nil
}
