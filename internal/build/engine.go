package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Spec is one image build.
type Spec struct {
	// ContextDir is the build context (the checked-out repo).
	ContextDir string
	// Dockerfile is the path of the Dockerfile to use.
	Dockerfile string
	// Image is the full reference to push, e.g. "registry:5000/blog:<sha>".
	Image string
	// Platform, e.g. "linux/arm64". Empty means the builder's own platform.
	Platform string
	// CacheRef is a registry ref for the layer cache, or "" for none.
	CacheRef string
	// BuildArgs are passed as --build-arg / --opt build-arg:.
	BuildArgs map[string]string
}

// Engine builds a Spec and pushes the image. It returns the pushed image's
// digest ("sha256:…"), or "" if the engine could not report it.
type Engine interface {
	Build(ctx context.Context, s Spec, out io.Writer) (digest string, err error)
}

// BuildKit drives a buildkitd daemon through the buildctl CLI. This is what
// runs in the cluster: buildkitd runs rootless next to the worker.
type BuildKit struct {
	// Addr of buildkitd, e.g. "tcp://buildkitd:1234" or "unix:///run/buildkit/buildkitd.sock".
	Addr string
	// Insecure allows pushing to a plain-HTTP registry (local development).
	Insecure bool
}

func (b BuildKit) Build(ctx context.Context, s Spec, out io.Writer) (string, error) {
	meta, cleanup, err := metadataFile()
	if err != nil {
		return "", err
	}
	defer cleanup()
	cmd := exec.CommandContext(ctx, "buildctl", b.args(s, meta)...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("buildctl: %w", err)
	}
	return readDigest(meta), nil
}

func (b BuildKit) args(s Spec, metadata string) []string {
	output := "type=image,name=" + s.Image + ",push=true"
	if b.Insecure {
		output += ",registry.insecure=true"
	}
	args := []string{
		"--addr", b.Addr,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + s.ContextDir,
		"--local", "dockerfile=" + filepath.Dir(s.Dockerfile),
		"--opt", "filename=" + filepath.Base(s.Dockerfile),
		"--output", output,
		"--progress", "plain",
		"--metadata-file", metadata,
	}
	if s.Platform != "" {
		args = append(args, "--opt", "platform="+s.Platform)
	}
	for _, k := range sortedKeys(s.BuildArgs) {
		args = append(args, "--opt", "build-arg:"+k+"="+s.BuildArgs[k])
	}
	if s.CacheRef != "" {
		// The registry cache lets a fresh buildkitd (new pod, new node) reuse
		// layers from earlier builds of the same app.
		cache := "type=registry,ref=" + s.CacheRef
		if b.Insecure {
			cache += ",registry.insecure=true"
		}
		args = append(args, "--export-cache", cache+",mode=max", "--import-cache", cache)
	}
	return args
}

// Docker builds with `docker buildx build --push`. Handy on a laptop, where
// Docker Desktop already ships BuildKit and there is no buildctl binary.
type Docker struct{}

func (Docker) Build(ctx context.Context, s Spec, out io.Writer) (string, error) {
	meta, cleanup, err := metadataFile()
	if err != nil {
		return "", err
	}
	defer cleanup()
	cmd := exec.CommandContext(ctx, "docker", Docker{}.args(s, meta)...)
	cmd.Stdout, cmd.Stderr = out, out
	// Plain progress is line-oriented, which is what the log table wants.
	cmd.Env = append(os.Environ(), "BUILDKIT_PROGRESS=plain")
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker buildx build: %w", err)
	}
	return readDigest(meta), nil
}

func (Docker) args(s Spec, metadata string) []string {
	args := []string{
		"buildx", "build",
		"--progress", "plain",
		"--file", s.Dockerfile,
		"--tag", s.Image,
		"--push",
		"--metadata-file", metadata,
	}
	if s.Platform != "" {
		args = append(args, "--platform", s.Platform)
	}
	for _, k := range sortedKeys(s.BuildArgs) {
		args = append(args, "--build-arg", k+"="+s.BuildArgs[k])
	}
	return append(args, s.ContextDir)
}

func metadataFile() (string, func(), error) {
	f, err := os.CreateTemp("", "paas-build-meta-*.json")
	if err != nil {
		return "", nil, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// readDigest pulls the pushed image digest out of a --metadata-file.
func readDigest(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var meta struct {
		Digest string `json:"containerimage.digest"`
	}
	if json.Unmarshal(b, &meta) != nil {
		return ""
	}
	return meta.Digest
}

// binary is the executable an engine shells out to.
func binary(e Engine) string {
	switch e.(type) {
	case BuildKit, *BuildKit:
		return "buildctl"
	case Docker, *Docker:
		return "docker"
	}
	return ""
}
