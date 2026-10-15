package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

func digest(n int) string { return fmt.Sprintf("sha256:%064x", n) }

// fakeRegistry implements the part of the distribution API the client uses:
// manifests by tag and digest, deletes by digest (which drop every tag of
// the manifest, as registry:2 and ECR do) and paginated tag lists.
type fakeRegistry struct {
	mu        sync.Mutex
	tags      map[string]map[string]string // repo → tag → digest
	deletes   []string                     // "repo@digest"
	noDelete  bool                         // registry:2 without REGISTRY_STORAGE_DELETE_ENABLED
	auth      func(w http.ResponseWriter, r *http.Request) bool
	pageSize  int
	requests  []string
	lastAuthz string
}

func newFake() *fakeRegistry {
	return &fakeRegistry{tags: map[string]map[string]string{}, pageSize: 2}
}

func (f *fakeRegistry) push(repo, tag, d string) {
	if f.tags[repo] == nil {
		f.tags[repo] = map[string]string{}
	}
	f.tags[repo][tag] = d
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if f.auth != nil && !f.auth(w, r) {
		return
	}
	f.lastAuthz = r.Header.Get("Authorization")
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	switch {
	case strings.Contains(path, "/manifests/"):
		repo, ref, _ := strings.Cut(path, "/manifests/")
		tags := f.tags[repo]
		d := ref
		if !strings.HasPrefix(ref, "sha256:") {
			d = tags[ref]
		} else {
			found := false
			for _, v := range tags {
				found = found || v == ref
			}
			if !found {
				d = ""
			}
		}
		if d == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodHead:
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			if f.noDelete {
				w.WriteHeader(http.StatusMethodNotAllowed)
				fmt.Fprint(w, `{"errors":[{"code":"UNSUPPORTED","message":"The operation is unsupported."}]}`)
				return
			}
			if !strings.HasPrefix(ref, "sha256:") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for t, v := range tags {
				if v == d {
					delete(tags, t)
				}
			}
			f.deletes = append(f.deletes, repo+"@"+d)
			w.WriteHeader(http.StatusAccepted)
		}
	case strings.HasSuffix(path, "/tags/list"):
		repo := strings.TrimSuffix(path, "/tags/list")
		tags, ok := f.tags[repo]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var all []string
		for t := range tags {
			all = append(all, t)
		}
		sort.Strings(all)
		last := r.URL.Query().Get("last")
		var page []string
		for _, t := range all {
			if t > last && len(page) < f.pageSize {
				page = append(page, t)
			}
		}
		if len(page) == f.pageSize && page[len(page)-1] != all[len(all)-1] {
			w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?last=%s&n=%d>; rel="next"`, repo, page[len(page)-1], f.pageSize))
		}
		json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": page})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func clientFor(t *testing.T, f http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Client{Host: strings.TrimPrefix(srv.URL, "http://"), Insecure: true}, srv
}

func TestParse(t *testing.T) {
	d := digest(1)
	for _, tc := range []struct {
		in   string
		want Image
	}{
		{"localhost:5000/blog:abc-7@" + d, Image{"localhost:5000", "blog", "abc-7", d}},
		{"localhost:5000/blog:abc", Image{"localhost:5000", "blog", "abc", ""}},
		{"ghcr.io/nisagwn/blog@" + d, Image{"ghcr.io", "nisagwn/blog", "", d}},
		{"123.dkr.ecr.eu-central-1.amazonaws.com/paas/blog:f00-12@" + d,
			Image{"123.dkr.ecr.eu-central-1.amazonaws.com", "paas/blog", "f00-12", d}},
		{"localhost/blog:x", Image{"localhost", "blog", "x", ""}},
	} {
		got, err := Parse(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
		if err == nil && got.String() != tc.in {
			t.Errorf("String() = %q, want %q", got.String(), tc.in)
		}
	}
	for _, bad := range []string{"", "blog:abc", "nisagwn/blog:abc", "localhost:5000/blog", "r.io/blog@sha256:zz", "registry.local/"} {
		if img, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", bad, img)
		}
	}
	if got := (Image{Host: "r.io", Repository: "a/b"}).Name(); got != "r.io/a/b" {
		t.Errorf("Name = %q", got)
	}
}

func TestNew(t *testing.T) {
	c := New("123.dkr.ecr.eu-central-1.amazonaws.com/paas/", false)
	if c.Host != "123.dkr.ecr.eu-central-1.amazonaws.com" || c.Prefix != "paas" || c.Insecure {
		t.Errorf("ECR client = %+v", c)
	}
	if got := c.AppRepository("blog"); got != "paas/blog" {
		t.Errorf("AppRepository = %q", got)
	}
	if c := New("localhost:5000", false); !c.Insecure || c.AppRepository("blog") != "blog" {
		t.Errorf("local client = %+v", c)
	}
	if c := New("127.0.0.1:5000", false); !c.Insecure {
		t.Error("a loopback registry is plain HTTP")
	}
	if c := New("registry.example.com", true); !c.Insecure {
		t.Error("PAAS_REGISTRY_INSECURE")
	}
	if !c.Owns(Image{Host: "123.DKR.ecr.eu-central-1.amazonaws.com"}) || c.Owns(Image{Host: "localhost:5000"}) {
		t.Error("Owns compares hosts")
	}
}

func TestResolveAndDelete(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.push("blog", "abc-1", digest(1))
	f.push("blog", "abc-2", digest(1)) // a second deployment, same digest
	f.push("blog", "def-3", digest(2))
	c, _ := clientFor(t, f)

	d, err := c.Resolve(ctx, Image{Host: c.Host, Repository: "blog", Tag: "abc-1"})
	if err != nil || d != digest(1) {
		t.Fatalf("Resolve = %q, %v", d, err)
	}
	// A pinned reference needs no request.
	n := len(f.requests)
	if d, _ := c.Resolve(ctx, Image{Repository: "blog", Tag: "zzz", Digest: digest(9)}); d != digest(9) || len(f.requests) != n {
		t.Fatalf("pinned Resolve = %q", d)
	}
	if d, err := c.Resolve(ctx, Image{Repository: "blog", Tag: "gone"}); err != nil || d != "" {
		t.Fatalf("missing tag: %q, %v", d, err)
	}
	if d, err := c.Resolve(ctx, Image{Repository: "nope", Tag: "x"}); err != nil || d != "" {
		t.Fatalf("missing repository: %q, %v", d, err)
	}

	if err := c.Delete(ctx, "blog", digest(1)); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.tags["blog"]["abc-2"]; ok {
		t.Error("deleting a digest removes every tag pointing at it")
	}
	if f.tags["blog"]["def-3"] != digest(2) {
		t.Error("other manifests stay")
	}
	// Idempotent: deleting again is not an error.
	if err := c.Delete(ctx, "blog", digest(1)); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if err := c.Delete(ctx, "blog", "latest"); err == nil {
		t.Error("only digests are deleted")
	}

	f.noDelete = true
	if err := c.Delete(ctx, "blog", digest(2)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("delete disabled: %v", err)
	}
}

func TestTagsAndDeleteRepository(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	for i := 1; i <= 5; i++ {
		f.push("paas/blog", fmt.Sprintf("t%d", i), digest(i%3)) // shared digests
	}
	f.push("paas/blog", "buildcache", digest(7))
	f.push("paas/other", "x", digest(8))
	c, _ := clientFor(t, f)
	c.Prefix = "paas"

	tags, err := c.Tags(ctx, "paas/blog")
	if err != nil || len(tags) != 6 {
		t.Fatalf("tags over pages = %v, %v", tags, err)
	}
	if tags, err := c.Tags(ctx, "paas/none"); err != nil || len(tags) != 0 {
		t.Fatalf("missing repository: %v, %v", tags, err)
	}

	n, err := c.DeleteAppImages(ctx, "blog")
	if err != nil || n != 4 { // digests 0, 1, 2 and the cache
		t.Fatalf("DeleteAppImages = %d, %v (deletes %v)", n, err, f.deletes)
	}
	if len(f.tags["paas/blog"]) != 0 || len(f.tags["paas/other"]) != 1 {
		t.Fatalf("left: %v", f.tags)
	}
	// Repeating finds nothing; a registry without deletes is not an error.
	if n, err := c.DeleteAppImages(ctx, "blog"); err != nil || n != 0 {
		t.Fatalf("again: %d, %v", n, err)
	}
	f.noDelete = true
	if _, err := c.DeleteAppImages(ctx, "other"); err != nil {
		t.Fatalf("delete disabled: %v", err)
	}
}

func writeDockerConfig(t *testing.T, host, user, pass string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`{"auths":{"https://%s/v1/":{"auth":%q},"other.io":{"auth":"eDp5"}}}`,
		host, base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ECR answers with a Basic challenge; the user is "AWS" and the password
// the token the paas-ecr-auth timer writes into the Docker config.
func TestBasicChallenge(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.push("paas/blog", "a-1", digest(1))
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("AWS:ecr-token"))
	f.auth = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") == want {
			return true
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="https://123.dkr.ecr.eu-central-1.amazonaws.com/",service="ecr.amazonaws.com"`)
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	c, _ := clientFor(t, f)
	dir := writeDockerConfig(t, c.Host, "AWS", "ecr-token")
	c.Credentials = DockerConfig(dir)
	if err := c.Delete(ctx, "paas/blog", digest(1)); err != nil {
		t.Fatal(err)
	}
	if len(f.deletes) != 1 {
		t.Fatalf("deletes = %v", f.deletes)
	}
	// The token is read again on every call: a refreshed one is used at once.
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths":{}}`), 0o600)
	if _, err := c.Tags(ctx, "paas/blog"); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("without credentials: %v", err)
	}
}

// Docker Hub, GHCR and Harbor answer with a Bearer challenge: the client
// fetches a token for pull, push and delete on the repository.
func TestBearerChallenge(t *testing.T) {
	ctx := context.Background()
	var scope, basic string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope, basic = r.URL.Query().Get("scope")+" "+r.URL.Query().Get("service"), r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]string{"token": "tok-1"})
	}))
	defer tokenSrv.Close()
	f := newFake()
	f.push("nisagwn/blog", "a-1", digest(1))
	f.auth = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") == "Bearer tok-1" {
			return true
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="reg.test",scope="repository:nisagwn/blog:pull"`, tokenSrv.URL))
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	c, _ := clientFor(t, f)
	c.Credentials = DockerConfig(writeDockerConfig(t, c.Host, "nisa", "pat"))
	d, err := c.Resolve(ctx, Image{Repository: "nisagwn/blog", Tag: "a-1"})
	if err != nil || d != digest(1) {
		t.Fatalf("Resolve = %q, %v", d, err)
	}
	if scope != "repository:nisagwn/blog:pull,push,delete reg.test" {
		t.Errorf("token request scope/service = %q", scope)
	}
	if basic != "Basic "+base64.StdEncoding.EncodeToString([]byte("nisa:pat")) {
		t.Errorf("token request auth = %q", basic)
	}
}

func TestParseChallenge(t *testing.T) {
	s, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:a/b:pull,push"`)
	if s != "Bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" ||
		p["scope"] != "repository:a/b:pull,push" {
		t.Errorf("%q %v", s, p)
	}
	if s, p := parseChallenge(`Basic realm=x`); s != "Basic" || p["realm"] != "x" {
		t.Errorf("%q %v", s, p)
	}
}
