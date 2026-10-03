// Package build turns a commit into a container image in a registry:
//
//	shallow clone at the exact SHA → detect project type → (generate
//	Dockerfile) → BuildKit build → push <registry>/<app>:<sha>
//
// Every line of output is streamed into the deployment's log.
package build

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nisagwn/minipaas/internal/naming"
	"github.com/nisagwn/minipaas/internal/store"
	"github.com/nisagwn/minipaas/internal/worker"
)

// Builder implements worker.Builder.
type Builder struct {
	Engine Engine
	// Registry is the image prefix, e.g. "localhost:5000",
	// "ghcr.io/nisagwn" or "<account>.dkr.ecr.<region>.amazonaws.com".
	Registry string
	// Platform to build for, e.g. "linux/arm64". Empty = the builder's own.
	Platform string
	// Cache enables a registry layer cache at <registry>/<app>:buildcache.
	Cache bool
	// GitBaseURL is joined with the app's "owner/repo" to get the clone URL,
	// e.g. "https://github.com". A local directory works too (tests, demos).
	GitBaseURL string
	// GitToken authenticates clones of private repositories (optional).
	GitToken string
	// WorkDir holds per-build checkouts. Empty means the OS temp directory.
	WorkDir string
}

var _ worker.Builder = (*Builder)(nil)

// Check verifies the binaries the builder shells out to, so that a
// misconfiguration shows up at startup instead of on the first push.
func (b *Builder) Check() error {
	bins := []string{"git"}
	if bin := binary(b.Engine); bin != "" {
		bins = append(bins, bin)
	}
	for _, bin := range bins {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("build: %q not found in PATH", bin)
		}
	}
	if b.Registry == "" {
		return fmt.Errorf("build: registry is not set")
	}
	return nil
}

// ImageName is where the image of a commit is pushed: <registry>/<app>:<sha>.
func (b *Builder) ImageName(app, sha string) string {
	return strings.TrimSuffix(b.Registry, "/") + "/" + app + ":" + sha
}

func (b *Builder) Build(ctx context.Context, d store.Deployment, log worker.Logger) (string, error) {
	work, err := os.MkdirTemp(b.WorkDir, fmt.Sprintf("minipaas-%d-", d.ID))
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "src")

	out := newLineWriter(func(line string) { log("%s", line) })
	defer out.Flush()

	// 1. Source
	repoURL := strings.TrimSuffix(b.GitBaseURL, "/") + "/" + d.Repo
	log("==> fetching %s@%s", d.Repo, naming.ShortSHA(d.CommitSHA))
	start := time.Now()
	if err := Checkout(ctx, repoURL, d.CommitSHA, b.GitToken, src, out); err != nil {
		out.Flush()
		return "", fmt.Errorf("fetch source: %w", err)
	}
	log("    done in %s", since(start))

	// 2. Detect
	plan, err := Detect(src)
	if err != nil {
		return "", err
	}
	log("==> detected: %s", plan.Summary)
	dockerfile := filepath.Join(src, "Dockerfile")
	if plan.Dockerfile != "" {
		// Kept outside the build context so it cannot clash with repo files.
		dockerfile = filepath.Join(work, "minipaas.Dockerfile")
		if err := os.WriteFile(dockerfile, []byte(plan.Dockerfile), 0o644); err != nil {
			return "", err
		}
		log("==> generated Dockerfile:")
		for _, l := range strings.Split(strings.TrimRight(plan.Dockerfile, "\n"), "\n") {
			log("    | %s", l)
		}
	}

	// 3. Build and push
	image := b.ImageName(d.AppName, d.CommitSHA)
	spec := Spec{
		ContextDir: src,
		Dockerfile: dockerfile,
		Image:      image,
		Platform:   b.Platform,
		BuildArgs: map[string]string{
			"MINIPAAS_APP":        d.AppName,
			"MINIPAAS_COMMIT_SHA": d.CommitSHA,
			"MINIPAAS_BRANCH":     d.Branch,
		},
	}
	if b.Cache {
		spec.CacheRef = strings.TrimSuffix(b.Registry, "/") + "/" + d.AppName + ":buildcache"
	}
	platform := b.Platform
	if platform == "" {
		platform = "native"
	}
	log("==> building %s (platform %s)", image, platform)
	start = time.Now()
	digest, err := b.Engine.Build(ctx, spec, out)
	out.Flush()
	if err != nil {
		return "", err
	}
	log("==> pushed %s in %s", image, since(start))

	// Pin the exact bytes we built: tags can be overwritten, digests cannot.
	if digest != "" {
		image += "@" + digest
	}
	return image, nil
}

func since(t time.Time) time.Duration {
	return time.Since(t).Round(100 * time.Millisecond)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
