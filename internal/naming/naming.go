// Package naming builds DNS-safe hostnames and Kubernetes-safe resource names.
//
// Every hostname is a single label under the platform domain so that one
// wildcard certificate (*.domain) covers all of them:
//
//	deployment URL : <sha7>-<app>.<domain>     (immutable, one per commit)
//	production     : <app>.<domain>            (alias)
//	preview        : <branch>-<app>.<domain>   (alias, one per branch)
package naming

import (
	"regexp"
	"strings"
)

// maxLabel is the DNS limit for a single hostname label.
const maxLabel = 63

var (
	appNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	nonAlnum  = regexp.MustCompile(`[^a-z0-9]+`)
)

// ValidAppName reports whether name can be used as an app name.
// It must stay in sync with the CHECK constraint on apps.name.
func ValidAppName(name string) bool {
	return appNameRe.MatchString(name) && !strings.HasSuffix(name, "-")
}

// ShortSHA returns the first 7 characters of a commit SHA.
func ShortSHA(sha string) string {
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}

// Slug lowercases s and collapses anything that is not [a-z0-9] into "-".
func Slug(s string) string {
	s = nonAlnum.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}

// DeploymentHost is the immutable URL of one deployment.
func DeploymentHost(sha, app, domain string) string {
	return ShortSHA(sha) + "-" + app + "." + domain
}

// ProductionHost is the production alias of an app.
func ProductionHost(app, domain string) string {
	return app + "." + domain
}

// PreviewHost is the alias that follows the latest deployment of a branch.
// The branch slug is truncated so the label never exceeds 63 characters.
func PreviewHost(branch, app, domain string) string {
	b := Slug(branch)
	if b == "" {
		b = "branch"
	}
	if room := maxLabel - len(app) - 1; len(b) > room {
		b = strings.TrimRight(b[:room], "-")
	}
	return b + "-" + app + "." + domain
}

// ResourceName is the Kubernetes name used for a deployment's objects.
func ResourceName(sha string) string {
	return "d-" + ShortSHA(sha)
}

// Namespace is the Kubernetes namespace of an app.
func Namespace(app string) string {
	return "app-" + app
}

// URL joins a scheme and a host. An empty scheme means "https": the
// platform serves every route over TLS unless it runs on a cluster without
// the wildcard certificate (PAAS_INGRESS_TLS=false).
func URL(scheme, host string) string {
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + host
}
