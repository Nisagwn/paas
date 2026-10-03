package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// fakeGitHub records requests and serves a small in-memory comment store.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server
	c   *github.Client

	mu       sync.Mutex
	statuses []github.Status
	prQuery  string
	prs      []map[string]any
	comments map[int][]github.Comment // by PR number
	nextID   int64
	patched  []int64
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, comments: map[int][]github.Comment{}, nextID: 100}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/nisagwn/blog/statuses/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f.checkHeaders(r)
		if r.PathValue("sha") != sha {
			t.Errorf("status for sha %q", r.PathValue("sha"))
		}
		var s github.Status
		json.NewDecoder(r.Body).Decode(&s)
		f.mu.Lock()
		f.statuses = append(f.statuses, s)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1}`)
	})
	mux.HandleFunc("GET /repos/nisagwn/blog/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.checkHeaders(r)
		f.mu.Lock()
		f.prQuery = r.URL.RawQuery
		prs := f.prs
		f.mu.Unlock()
		if prs == nil {
			prs = []map[string]any{}
		}
		json.NewEncoder(w).Encode(prs)
	})
	mux.HandleFunc("GET /repos/nisagwn/blog/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		f.checkHeaders(r)
		n, _ := strconv.Atoi(r.PathValue("n"))
		f.mu.Lock()
		all := f.comments[n]
		f.mu.Unlock()
		// Two comments per page so pagination via the Link header is exercised.
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		lo, hi := min((page-1)*2, len(all)), min(page*2, len(all))
		if hi < len(all) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/nisagwn/blog/issues/%d/comments?per_page=2&page=%d>; rel="next", <%s/x>; rel="last"`,
				f.srv.URL, n, page+1, f.srv.URL))
		}
		json.NewEncoder(w).Encode(all[lo:hi])
	})
	mux.HandleFunc("POST /repos/nisagwn/blog/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		f.checkHeaders(r)
		n, _ := strconv.Atoi(r.PathValue("n"))
		var in struct{ Body string }
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.nextID++
		c := github.Comment{ID: f.nextID, Body: in.Body, HTMLURL: fmt.Sprintf("https://github.test/c/%d", f.nextID)}
		f.comments[n] = append(f.comments[n], c)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(c)
	})
	mux.HandleFunc("PATCH /repos/nisagwn/blog/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.checkHeaders(r)
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var in struct{ Body string }
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.patched = append(f.patched, id)
		for n, cs := range f.comments {
			for i := range cs {
				if cs[i].ID == id {
					cs[i].Body = in.Body
					f.comments[n] = cs
					json.NewEncoder(w).Encode(cs[i])
					return
				}
			}
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.c = github.New(f.srv.URL+"/", "tok", slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f
}

func (f *fakeGitHub) checkHeaders(r *http.Request) {
	f.t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer tok" {
		f.t.Errorf("Authorization = %q", got)
	}
	if r.Header.Get("User-Agent") == "" || r.Header.Get("Accept") != "application/vnd.github+json" {
		f.t.Errorf("missing User-Agent/Accept headers: %v", r.Header)
	}
}

func (f *fakeGitHub) addComment(pr int, body string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.comments[pr] = append(f.comments[pr], github.Comment{ID: f.nextID, Body: body})
	return f.nextID
}

func TestCreateCommitStatus(t *testing.T) {
	f := newFake(t)
	long := strings.Repeat("ğ", 200)
	err := f.c.CreateCommitStatus(context.Background(), "nisagwn/blog", sha, github.Status{
		State: github.StateFailure, TargetURL: "https://paas.test/deployments/1", Description: long,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := f.statuses[0]
	if s.State != "failure" || s.Context != "paas/deploy" || s.TargetURL != "https://paas.test/deployments/1" {
		t.Fatalf("payload = %+v", s)
	}
	if n := utf8.RuneCountInString(s.Description); n != github.MaxDescription || !strings.HasSuffix(s.Description, "…") {
		t.Fatalf("description has %d runes: %q", n, s.Description)
	}

	if err := f.c.CreateCommitStatus(context.Background(), "nisagwn/blog", sha, github.Status{State: "done"}); err == nil {
		t.Fatal("invalid state accepted")
	}
	if err := f.c.CreateCommitStatus(context.Background(), "blog", sha, github.Status{State: "success"}); err == nil {
		t.Fatal("invalid repo accepted")
	}
}

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"kısa", 140, "kısa"},
		{"abcdef", 6, "abcdef"},
		{"abcdefg", 6, "abcde…"},
		{"abcd efg", 6, "abcd…"},
		{"çğüşöı", 3, "çğ…"},
	} {
		if got := github.Truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestOpenPullRequestsQuery(t *testing.T) {
	f := newFake(t)
	f.prs = []map[string]any{{"number": 7, "head": map[string]any{"ref": "feature/x", "sha": sha}}}
	prs, err := f.c.OpenPullRequests(context.Background(), "nisagwn/blog", "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].Number != 7 || prs[0].Head.SHA != sha {
		t.Fatalf("prs = %+v", prs)
	}
	if want := "head=nisagwn%3Afeature%2Fx&per_page=100&state=open"; f.prQuery != want {
		t.Fatalf("query = %q, want %q", f.prQuery, want)
	}
}

func TestUpsertCommentCreatesThenUpdates(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	marker := github.CommentMarker("blog")
	// Unrelated comments, including another app's marker, spread over pages.
	f.addComment(7, "LGTM")
	f.addComment(7, github.CommentMarker("blog-api")+" other app")
	f.addComment(7, "nit")

	c, created, err := f.c.UpsertComment(ctx, "nisagwn/blog", 7, marker, marker+"\nv1")
	if err != nil || !created {
		t.Fatalf("first upsert: created=%v err=%v", created, err)
	}
	c2, created, err := f.c.UpsertComment(ctx, "nisagwn/blog", 7, marker, marker+"\nv2")
	if err != nil || created {
		t.Fatalf("second upsert: created=%v err=%v", created, err)
	}
	if c2.ID != c.ID || len(f.patched) != 1 || f.patched[0] != c.ID {
		t.Fatalf("updated %v, want comment %d", f.patched, c.ID)
	}
	if got := f.comments[7]; len(got) != 4 || got[3].Body != marker+"\nv2" || got[1].Body != github.CommentMarker("blog-api")+" other app" {
		t.Fatalf("comments = %+v", got)
	}

	if _, _, err := f.c.UpsertComment(ctx, "nisagwn/blog", 7, marker, "no marker"); err == nil {
		t.Fatal("body without marker accepted")
	}
}

func TestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/validation/statuses/" + sha:
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Validation Failed","errors":[{"resource":"Status","field":"state","code":"invalid"}]}`)
		case "/repos/o/limited/statuses/" + sha:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "9999999999")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		case "/repos/o/secondary/statuses/" + sha:
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit"}`)
		default:
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "<html>bad gateway</html>")
		}
	}))
	defer srv.Close()
	c := github.New(srv.URL, "tok", nil)
	ctx := context.Background()
	st := github.Status{State: github.StateSuccess}

	var e *github.Error
	err := c.CreateCommitStatus(ctx, "o/validation", sha, st)
	if !errors.As(err, &e) || e.StatusCode != 422 || e.Message != "Validation Failed: state invalid" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "POST /repos/o/validation/statuses/"+sha+": 422 Validation Failed") {
		t.Fatalf("message = %q", err)
	}

	err = c.CreateCommitStatus(ctx, "o/limited", sha, st)
	if !errors.As(err, &e) || e.StatusCode != 403 || e.RetryAfter <= 0 || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("primary rate limit: %v", err)
	}
	err = c.CreateCommitStatus(ctx, "o/secondary", sha, st)
	if !errors.As(err, &e) || e.StatusCode != 429 || e.RetryAfter.Seconds() != 60 {
		t.Fatalf("secondary rate limit: %v", err)
	}
	err = c.CreateCommitStatus(ctx, "o/down", sha, st)
	if !errors.As(err, &e) || e.StatusCode != 502 || e.Message != "<html>bad gateway</html>" {
		t.Fatalf("non-JSON error: %v", err)
	}
}

// Link headers pointing at another host are not followed, so the token
// cannot leak there.
func TestPaginationStaysOnHost(t *testing.T) {
	var foreign bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreign = true
		fmt.Fprint(w, `[]`)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
			return
		}
		w.Header().Set("Link", `<`+other.URL+`/repos/o/r/issues/1/comments?page=2>; rel="next"`)
		fmt.Fprint(w, `[{"id":5,"body":"hi"}]`)
	}))
	defer srv.Close()
	c := github.New(srv.URL, "tok", nil)
	if _, created, err := c.UpsertComment(context.Background(), "o/r", 1, "<!-- m -->", "<!-- m --> x"); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if foreign {
		t.Fatal("followed a Link to another host")
	}
}

func deployment() store.Deployment {
	return store.Deployment{ID: 42, AppName: "blog", Repo: "nisagwn/blog", CommitSHA: sha,
		Branch: "feature/x", CommitMessage: "add | dark <mode>"}
}

func TestNotifierLifecycle(t *testing.T) {
	f := newFake(t)
	f.prs = []map[string]any{
		{"number": 7, "head": map[string]any{"ref": "feature/x", "sha": sha}},
		// PR whose head already moved to a newer commit: left alone.
		{"number": 8, "head": map[string]any{"ref": "feature/x", "sha": strings.Repeat("f", 40)}},
	}
	n := &github.Notifier{Client: f.c, PublicURL: "https://paas.test/"}
	ctx := context.Background()
	d := deployment()

	if err := n.DeploymentStarted(ctx, d); err != nil {
		t.Fatal(err)
	}
	ready := worker.Result{Status: store.StatusReady, URL: "https://0123456-blog.paas.test",
		PreviewURL: "https://feature-x-blog.paas.test"}
	if err := n.DeploymentFinished(ctx, d, ready); err != nil {
		t.Fatal(err)
	}
	failed := worker.Result{Status: store.StatusFailed, Error: "build: exit status 1\n" + strings.Repeat("x", 300)}
	if err := n.DeploymentFinished(ctx, d, failed); err != nil {
		t.Fatal(err)
	}

	if len(f.statuses) != 3 {
		t.Fatalf("statuses = %+v", f.statuses)
	}
	want := []struct{ state, target, desc string }{
		{"pending", "https://paas.test/deployments/42", "Build başladı"},
		{"success", "https://0123456-blog.paas.test", "Deploy hazır: 0123456-blog.paas.test"},
		{"failure", "https://paas.test/deployments/42", "Deploy başarısız: build: exit status 1 xxx"},
	}
	for i, w := range want {
		s := f.statuses[i]
		if s.State != w.state || s.TargetURL != w.target || !strings.HasPrefix(s.Description, w.desc) || s.Context != "paas/deploy" {
			t.Errorf("status %d = %+v, want %+v", i, s, w)
		}
	}
	if n := utf8.RuneCountInString(f.statuses[2].Description); n > github.MaxDescription {
		t.Errorf("failure description has %d runes", n)
	}

	// One comment on PR 7, created on success and updated in place on failure.
	if len(f.comments[8]) != 0 || len(f.comments[7]) != 1 || len(f.patched) != 1 {
		t.Fatalf("comments = %+v, patched = %v", f.comments, f.patched)
	}
	body := f.comments[7][0].Body
	for _, s := range []string{github.CommentMarker("blog"), "❌ Deploy başarısız", "exit status 1",
		"[Deployment #42](https://paas.test/deployments/42)", "`0123456` add \\| dark &lt;mode&gt;"} {
		if !strings.Contains(body, s) {
			t.Errorf("failure comment lacks %q:\n%s", s, body)
		}
	}
}

func TestCommentBodyReady(t *testing.T) {
	n := &github.Notifier{PublicURL: "https://paas.test"}
	d := deployment()
	d.Branch = "main"
	body := n.CommentBody(d, worker.Result{Status: store.StatusReady, URL: "https://0123456-blog.paas.test",
		PreviewURL: "https://main-blog.paas.test", ProductionURL: "https://blog.paas.test"})
	for _, s := range []string{
		"<!-- paas:preview:blog -->\n", "✅ Preview hazır",
		"| **Preview** | https://main-blog.paas.test |", "| **Production** | https://blog.paas.test |",
		"| **Deployment** | https://0123456-blog.paas.test |", "| **Durum** | ✅ Hazır |",
	} {
		if !strings.Contains(body, s) {
			t.Errorf("comment lacks %q:\n%s", s, body)
		}
	}
}

// A GitHub outage is reported as an error (the worker logs it), not a panic.
func TestNotifierSurfacesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()
	n := &github.Notifier{Client: github.New(srv.URL, "tok", nil)}
	err := n.DeploymentFinished(context.Background(), deployment(), worker.Result{Status: store.StatusReady})
	if err == nil || !strings.Contains(err.Error(), "404 Not Found") || !strings.Contains(err.Error(), "/pulls") {
		t.Fatalf("err = %v", err)
	}
}
