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

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
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
	// GitTokens, when set, replaces GitToken with a token per repository:
	// the GitHub App's installation token (Faz 15). An empty token clones
	// anonymously.
	GitTokens TokenSource
	// WorkDir holds per-build checkouts. Empty means the OS temp directory.
	WorkDir string
}

var _ worker.Builder = (*Builder)(nil)

// TokenSource returns the token that grants access to a repository
// ("owner/name"); github.AppTokens implements it.
type TokenSource interface {
	RepoToken(ctx context.Context, repo string) (string, error)
}

// gitToken is the clone token for repo.
func (b *Builder) gitToken(ctx context.Context, repo string) (string, error) {
	if b.GitTokens == nil {
		return b.GitToken, nil
	}
	return b.GitTokens.RepoToken(ctx, repo)
}

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

// Build clones the commit, detects the project under the root directory
// setting and builds and pushes its image.
func (b *Builder) Build(ctx context.Context, d store.Deployment, s store.BuildSettings, log worker.Logger) (worker.BuildResult, error) {
	image, framework, err := b.build(ctx, d, s, log)
	return worker.BuildResult{Image: image, Framework: framework}, err
}

func (b *Builder) build(ctx context.Context, d store.Deployment, s store.BuildSettings, log worker.Logger) (image, framework string, err error) {
	// Settings were validated when saved; check again, they end up in a Dockerfile.
	if s, err = NormalizeSettings(s); err != nil {
		return "", "", fmt.Errorf("build settings: %w", err)
	}
	work, err := os.MkdirTemp(b.WorkDir, fmt.Sprintf("paas-%d-", d.ID))
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "src")

	out := newLineWriter(func(line string) { log("%s", line) })
	defer out.Flush()

	// 1. Source
	repoURL := strings.TrimSuffix(b.GitBaseURL, "/") + "/" + d.Repo
	log("==> fetching %s@%s", d.Repo, naming.ShortSHA(d.CommitSHA))
	start := time.Now()
	token, err := b.gitToken(ctx, d.Repo)
	if err != nil {
		return "", "", fmt.Errorf("fetch source: repository token: %w", err)
	}
	if err := Checkout(ctx, repoURL, d.CommitSHA, token, src, out); err != nil {
		out.Flush()
		return "", "", fmt.Errorf("fetch source: %w", err)
	}
	log("    done in %s", since(start))

	// 2. Detect, inside the root directory (Faz 16)
	contextDir, err := rootDir(src, s.RootDirectory)
	if err != nil {
		return "", "", err
	}
	if s.RootDirectory != "" {
		log("==> root directory: %s", s.RootDirectory)
	}
	opts := OptionsFrom(s)
	plan, err := DetectWith(contextDir, opts)
	if err != nil {
		return "", "", err
	}
	log("==> framework: %s", plan.Framework)
	log("==> detected: %s", plan.Summary)
	if plan.Kind == KindDockerfile && opts.overridesCommands() {
		log("WARNING: the repository's Dockerfile is used as is; install/build/start commands, output directory " +
			"and node version settings apply to generated Dockerfiles only (set a framework to generate one)")
	}
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	if plan.Dockerfile != "" {
		// Kept outside the build context so it cannot clash with repo files.
		dockerfile = filepath.Join(work, "paas.Dockerfile")
		if err := os.WriteFile(dockerfile, []byte(plan.Dockerfile), 0o644); err != nil {
			return "", "", err
		}
		log("==> generated Dockerfile:")
		for _, l := range strings.Split(strings.TrimRight(plan.Dockerfile, "\n"), "\n") {
			log("    | %s", l)
		}
	}

	// 3. Build and push
	image = b.ImageName(d.AppName, d.CommitSHA)
	spec := Spec{
		ContextDir: contextDir,
		Dockerfile: dockerfile,
		Image:      image,
		Platform:   b.Platform,
		BuildArgs: map[string]string{
			"PAAS_APP":        d.AppName,
			"PAAS_COMMIT_SHA": d.CommitSHA,
			"PAAS_BRANCH":     d.Branch,
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
		return "", plan.Framework, err
	}
	log("==> pushed %s in %s", image, since(start))

	// Pin the exact bytes we built: tags can be overwritten, digests cannot.
	if digest != "" {
		image += "@" + digest
	}
	return image, plan.Framework, nil
}

// rootDir resolves the root directory setting inside the checkout src. A
// symlink may not lead the build context out of the repository.
func rootDir(src, root string) (string, error) {
	if root == "" {
		return src, nil
	}
	notFound := fmt.Errorf("root directory %q not found in the repository", root)
	dir := filepath.Join(src, filepath.FromSlash(root))
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return "", notFound
	}
	base, err1 := filepath.EvalSymlinks(src)
	resolved, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil {
		return "", notFound
	}
	if rel, err := filepath.Rel(base, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("root directory %q points outside the repository", root)
	}
	return resolved, nil
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
