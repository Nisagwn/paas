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
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
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

// DeploymentKey identifies one deployment of a commit (Faz 17): the short
// SHA for the commit's first deployment (generation 0), "<sha7>-<n>" for
// its n-th redeploy or promotion.
func DeploymentKey(sha string, generation int) string {
	if generation <= 0 {
		return ShortSHA(sha)
	}
	return ShortSHA(sha) + "-" + strconv.Itoa(generation)
}

// ObjectName is the Kubernetes name of a deployment's objects;
// ObjectName(sha, 0) == ResourceName(sha).
func ObjectName(sha string, generation int) string {
	return "d-" + DeploymentKey(sha, generation)
}

// InstanceHost is the immutable URL of a deployment of the given
// generation; InstanceHost(sha, 0, ...) == DeploymentHost(sha, ...).
func InstanceHost(sha string, generation int, app, domain string) string {
	return DeploymentKey(sha, generation) + "-" + app + "." + domain
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

// ---- Faz 22: add-ons ----

// AddonObject is the Kubernetes name of an add-on's objects (StatefulSet,
// Service, Secret) in the app's namespace.
func AddonObject(addon string) string {
	return "addon-" + addon
}

// maxIdent is PostgreSQL's identifier limit (NAMEDATALEN - 1).
const maxIdent = 63

// BranchDatabase is the database (and owner role) of a preview branch's
// copy: "preview_" + the branch slug with "_" separators. A branch whose
// name changed on the way (other characters, upper case) or had to be cut
// gets a hash of the full name appended, so feature/x and feature-x never
// share a database. The result is a valid unquoted identifier.
func BranchDatabase(branch string) string {
	const prefix = "preview_"
	slug := strings.ReplaceAll(Slug(branch), "-", "_")
	exact := slug == branch
	if slug == "" {
		slug, exact = "branch", false
	}
	if len(prefix)+len(slug) > maxIdent {
		exact = false
	}
	if exact {
		return prefix + slug
	}
	sum := sha256.Sum256([]byte(branch))
	suffix := "_" + hex.EncodeToString(sum[:])[:8]
	if room := maxIdent - len(prefix) - len(suffix); len(slug) > room {
		slug = strings.TrimRight(slug[:room], "_")
	}
	return prefix + slug + suffix
}
