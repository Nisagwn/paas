package build

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestDockerBuildExamples really builds every app in examples/, pushes it to
// the local registry, runs it and checks that it answers on the platform
// port. It needs Docker and `make registry`, so it only runs when
// PAAS_TEST_DOCKER_BUILD=1 (see `make test-build`).
func TestDockerBuildExamples(t *testing.T) {
	if os.Getenv("PAAS_TEST_DOCKER_BUILD") == "" {
		t.Skip("set PAAS_TEST_DOCKER_BUILD=1 to run real image builds")
	}
	registry := os.Getenv("PAAS_REGISTRY")
	if registry == "" {
		registry = "localhost:5000"
	}
	examples := map[string]string{
		"node-hello":   "hello from node",
		"go-hello":     "hello from go",
		"static-site":  "hello from a static site",
		"python-hello": "hello from python",
		"ruby-hello":   "hello from ruby",
		"java-hello":   "hello from java",
	}
	for name, want := range examples {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join("..", "..", "examples", name)
			plan, err := Detect(src)
			if err != nil {
				t.Fatal(err)
			}
			dockerfile := filepath.Join(t.TempDir(), "Dockerfile")
			os.WriteFile(dockerfile, []byte(plan.Dockerfile), 0o644)

			image := registry + "/example-" + name + ":test"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			var logs logSink
			out := newLineWriter(func(s string) { logs.log("%s", s) })
			digest, err := Docker{}.Build(ctx, Spec{ContextDir: src, Dockerfile: dockerfile, Image: image}, out)
			out.Flush()
			if err != nil {
				t.Fatalf("%v\n%s", err, logs.text())
			}
			if !strings.HasPrefix(digest, "sha256:") {
				t.Errorf("digest = %q", digest)
			}

			ref := image + "@" + digest
			// runAsNonRoot needs a numeric user, set by the image or its base.
			exec.Command("docker", "pull", "-q", ref).Run()
			userOut, err := exec.Command("docker", "inspect", "--format", "{{.Config.User}}", ref).Output()
			if user := strings.TrimSpace(string(userOut)); err != nil || !numericUser.MatchString(user) {
				t.Fatalf("image user = %q (%v), want a numeric non-root uid", user, err)
			}

			got := runAndGet(t, ref)
			if !strings.Contains(got, want) {
				t.Fatalf("GET / = %q, want it to contain %q", got, want)
			}
		})
	}
}

// runAndGet starts the image (pulling it by digest from the registry) and
// returns the body of GET / on its port 8080.
func runAndGet(t *testing.T, image string) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "-d", "--rm", "-p", "127.0.0.1::8080", image).Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })

	out, err = exec.Command("docker", "port", id, "8080/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	addr := strings.TrimSpace(strings.Split(string(out), "\n")[0])

	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return string(b)
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			t.Fatalf("app never answered on :8080: %v\ncontainer logs:\n%s", err, logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// numericUser matches "uid" or "uid:gid" with a non-zero uid.
var numericUser = regexp.MustCompile(`^[1-9][0-9]*(:[0-9]+)?$`)
