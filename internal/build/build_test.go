package build

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nisagwn/paas/internal/store"
)

// gitRepo creates <base>/<owner>/<repo> with two commits and returns the
// base dir and both SHAs. Only the first commit has hello.txt = "v1".
func gitRepo(t *testing.T, repo string, files map[string]string) (base, first, second string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base = t.TempDir()
	dir := filepath.Join(base, filepath.FromSlash(repo))
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir,
			"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	os.MkdirAll(dir, 0o755)
	git("init", "-q", "-b", "main")
	files["hello.txt"] = "v1"
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	git("add", ".")
	git("commit", "-q", "-m", "first")
	first = git("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("v2"), 0o644)
	git("commit", "-q", "-am", "second")
	second = git("rev-parse", "HEAD")
	return base, first, second
}

func TestCheckoutExactCommit(t *testing.T) {
	base, first, _ := gitRepo(t, "o/r", map[string]string{})
	dir := filepath.Join(t.TempDir(), "src")
	// "file://" makes git honour --depth even for a local repository.
	url := "file://" + filepath.ToSlash(filepath.Join(base, "o", "r"))
	if err := Checkout(context.Background(), url, first, "", dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "hello.txt")); string(b) != "v1" {
		t.Fatalf("hello.txt = %q, want the first commit's v1", b)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatal(".git should be removed from the build context")
	}
}

func TestCheckoutUnknownCommit(t *testing.T) {
	base, _, _ := gitRepo(t, "o/r", map[string]string{})
	err := Checkout(context.Background(), filepath.Join(base, "o", "r"),
		strings.Repeat("0", 40), "", filepath.Join(t.TempDir(), "src"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "git fetch: fatal:") {
		t.Fatalf("err = %v, want git's own fatal message", err)
	}
}

// fakeEngine records the Spec and what the context dir looked like.
type fakeEngine struct {
	spec       Spec
	dockerfile string
	hello      string
	err        error
}

func (f *fakeEngine) Build(_ context.Context, s Spec, out io.Writer) (string, error) {
	f.spec = s
	b, _ := os.ReadFile(s.Dockerfile)
	f.dockerfile = string(b)
	h, _ := os.ReadFile(filepath.Join(s.ContextDir, "hello.txt"))
	f.hello = string(h)
	fmt.Fprint(out, "#1 [internal] load build definition\n#2 DONE 0.1s\n")
	if f.err != nil {
		return "", f.err
	}
	return "sha256:feed", nil
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) log(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) text() string { return strings.Join(l.lines, "\n") }

func TestBuilderBuild(t *testing.T) {
	base, first, _ := gitRepo(t, "nisagwn/blog", map[string]string{"index.html": "<h1>blog</h1>"})
	eng := &fakeEngine{}
	b := &Builder{Engine: eng, Registry: "localhost:5000/", Platform: "linux/arm64", Cache: true,
		GitBaseURL: base, WorkDir: t.TempDir()}
	d := store.Deployment{ID: 7, AppName: "blog", Repo: "nisagwn/blog", CommitSHA: first, Branch: "main"}

	var logs logSink
	res, err := b.Build(context.Background(), d, store.BuildSettings{}, logs.log)
	image := res.Image
	if res.Framework != "Static" {
		t.Errorf("framework = %q, want Static", res.Framework)
	}
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, logs.text())
	}

	if want := "localhost:5000/blog:" + first + "@sha256:feed"; image != want {
		t.Errorf("image = %q, want %q", image, want)
	}
	if eng.hello != "v1" {
		t.Errorf("built the wrong commit: hello.txt = %q", eng.hello)
	}
	if !strings.Contains(eng.dockerfile, "nginx") {
		t.Errorf("expected the generated static Dockerfile, got:\n%s", eng.dockerfile)
	}
	if eng.spec.Platform != "linux/arm64" || eng.spec.CacheRef != "localhost:5000/blog:buildcache" ||
		eng.spec.BuildArgs["PAAS_COMMIT_SHA"] != first {
		t.Errorf("spec = %+v", eng.spec)
	}
	for _, want := range []string{"==> fetching nisagwn/blog@" + first[:7], "==> detected: static site",
		"    | FROM ", "#2 DONE 0.1s", "==> pushed localhost:5000/blog:" + first} {
		if !strings.Contains(logs.text(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.text())
		}
	}
	// The per-build work directory is cleaned up.
	if left, _ := os.ReadDir(b.WorkDir); len(left) != 0 {
		t.Errorf("work dir not cleaned: %v", left)
	}
}

func TestBuilderFailures(t *testing.T) {
	base, first, _ := gitRepo(t, "o/empty", map[string]string{"README.md": "nothing to build"})
	d := store.Deployment{ID: 1, AppName: "empty", Repo: "o/empty", CommitSHA: first, Branch: "main"}

	b := &Builder{Engine: &fakeEngine{}, Registry: "r", GitBaseURL: base, WorkDir: t.TempDir()}
	if _, err := b.Build(context.Background(), d, store.BuildSettings{}, func(string, ...any) {}); err != ErrUnknownProject {
		t.Fatalf("err = %v, want ErrUnknownProject", err)
	}

	d.Repo = "o/missing"
	if _, err := b.Build(context.Background(), d, store.BuildSettings{}, func(string, ...any) {}); err == nil ||
		!strings.Contains(err.Error(), "fetch source") {
		t.Fatalf("err = %v, want a fetch error", err)
	}
}

// repoTokens is a TokenSource that records which repositories were asked for.
type repoTokens struct {
	mu    sync.Mutex
	token string
	err   error
	repos []string
}

func (r *repoTokens) RepoToken(_ context.Context, repo string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repos = append(r.repos, repo)
	return r.token, r.err
}

// authRecorder stands in for a Git host: it records the Authorization
// header of every request and answers 404.
func authRecorder(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func basicAuth(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// With a TokenSource (the GitHub App) the clone authenticates with the
// repository's token, not GitToken.
func TestBuilderRepoToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	srv, seen := authRecorder(t)
	tokens := &repoTokens{token: "inst-token"}
	b := &Builder{Engine: &fakeEngine{}, Registry: "r", GitBaseURL: srv.URL, GitToken: "pat",
		GitTokens: tokens, WorkDir: t.TempDir()}
	d := store.Deployment{ID: 1, AppName: "web", Repo: "acme/web", CommitSHA: strings.Repeat("a", 40), Branch: "main"}
	nolog := func(string, ...any) {}

	if _, err := b.Build(context.Background(), d, store.BuildSettings{}, nolog); err == nil {
		t.Fatal("clone from the 404 host succeeded")
	}
	if got := seen(); len(got) == 0 || got[0] != basicAuth("inst-token") {
		t.Fatalf("Authorization = %q, want the installation token", got)
	}
	if len(tokens.repos) != 1 || tokens.repos[0] != "acme/web" {
		t.Fatalf("token asked for %v", tokens.repos)
	}

	// No token for the repository: an anonymous clone (public repositories).
	srv2, seen2 := authRecorder(t)
	b.GitBaseURL, tokens.token = srv2.URL, ""
	b.Build(context.Background(), d, store.BuildSettings{}, nolog)
	if got := seen2(); len(got) == 0 || got[0] != "" {
		t.Fatalf("anonymous clone sent Authorization %q", got)
	}

	// A token that cannot be had fails the build before cloning.
	tokens.err = errors.New("github: 503")
	if _, err := b.Build(context.Background(), d, store.BuildSettings{}, nolog); err == nil ||
		!strings.Contains(err.Error(), "repository token: github: 503") {
		t.Fatalf("err = %v", err)
	}
}

// Without a TokenSource GitToken is used, as before the GitHub App.
func TestBuilderStaticToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	srv, seen := authRecorder(t)
	b := &Builder{Engine: &fakeEngine{}, Registry: "r", GitBaseURL: srv.URL, GitToken: "pat", WorkDir: t.TempDir()}
	d := store.Deployment{ID: 1, AppName: "web", Repo: "acme/web", CommitSHA: strings.Repeat("a", 40), Branch: "main"}
	b.Build(context.Background(), d, store.BuildSettings{}, func(string, ...any) {})
	if got := seen(); len(got) == 0 || got[0] != basicAuth("pat") {
		t.Fatalf("Authorization = %q, want the static token", got)
	}
}
