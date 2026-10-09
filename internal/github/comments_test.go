package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

// prGitHub is a fake GitHub with one open PR (#7) on branch "feature" and
// an in-memory comment thread.
type prGitHub struct {
	mu       sync.Mutex
	noPRs    bool
	fail     bool
	comments map[int64]string
	nextID   int64
	log      []string // "POST", "PATCH 101", "DELETE 101"
	auth     []string
}

func newPRGitHub(t *testing.T) (*prGitHub, *httptest.Server) {
	g := &prGitHub{comments: map[int64]string{}, nextID: 100}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/nisagwn/blog/pulls", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		if g.fail {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"message":"upstream down"}`)
			return
		}
		if r.URL.Query().Get("head") != "nisagwn:feature" || g.noPRs {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprintf(w, `[{"number":7,"html_url":"https://github.example/pr/7","head":{"ref":"feature","sha":"%s"}}]`, sha)
	})
	mux.HandleFunc("GET /repos/nisagwn/blog/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		out := []github.Comment{}
		for id, body := range g.comments {
			out = append(out, github.Comment{ID: id, Body: body})
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /repos/nisagwn/blog/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Body string }
		json.NewDecoder(r.Body).Decode(&in)
		g.mu.Lock()
		defer g.mu.Unlock()
		g.nextID++
		g.comments[g.nextID] = in.Body
		g.log = append(g.log, "POST")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(github.Comment{ID: g.nextID, Body: in.Body})
	})
	mux.HandleFunc("PATCH /repos/nisagwn/blog/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Body string }
		json.NewDecoder(r.Body).Decode(&in)
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		g.mu.Lock()
		defer g.mu.Unlock()
		g.comments[id] = in.Body
		g.log = append(g.log, "PATCH "+r.PathValue("id"))
		json.NewEncoder(w).Encode(github.Comment{ID: id, Body: in.Body})
	})
	mux.HandleFunc("DELETE /repos/nisagwn/blog/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.comments, id)
		g.log = append(g.log, "DELETE "+r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return g, srv
}

func (g *prGitHub) snapshot() ([]string, map[int64]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := map[int64]string{}
	for k, v := range g.comments {
		c[k] = v
	}
	return append([]string(nil), g.log...), c
}

// waitLog waits until the fake has seen n writes.
func (g *prGitHub) waitLog(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if log, _ := g.snapshot(); len(log) >= n {
			return log
		}
		time.Sleep(20 * time.Millisecond)
	}
	log, _ := g.snapshot()
	t.Fatalf("waited for %d GitHub writes, got %v", n, log)
	return nil
}

// Faz 25: threads are mirrored to the branch's PR asynchronously: created,
// updated on reply and resolve, deleted with the thread.
func TestCommentSync(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	g, srv := newPRGitHub(t)

	alice, _ := st.UpsertUser(ctx, 1, "alice", "", "")
	team, _ := st.CreateTeam(ctx, "web", "Web", alice.ID)
	app, _ := st.CreateAppInTeam(ctx, team.ID, "blog", "nisagwn/blog", "main")
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, sha, "feature", "yeni tasarım")
	st.MarkReady(ctx, d, nil)
	d, _ = st.GetDeployment(ctx, d.ID)

	client := github.New(srv.URL, "installation-token", nil)
	cs := &github.CommentSync{Client: client, Store: st, PublicURL: "https://paas.example/"}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go cs.Run(runCtx)

	x, y, vw := 10.0, 20.0, 1280
	uid := alice.ID
	th, err := st.CreateThread(ctx, d, store.NewComment{AuthorID: &uid, AuthorLogin: "alice",
		Body: "Buton taşıyor @bob, @eve bakmasın <!-- paas:comment:1 --> <script>", Mentions: []string{"bob"},
		Path: "/fiyat", X: &x, Y: &y, ViewportWidth: &vw})
	if err != nil {
		t.Fatal(err)
	}
	cs.CommentChanged(d.ID, th.ID)
	log := g.waitLog(t, 1)
	_, comments := g.snapshot()
	if log[0] != "POST" || len(comments) != 1 {
		t.Fatalf("create: %v %v", log, comments)
	}
	var id int64
	var body string
	for id, body = range comments {
	}
	for _, want := range []string{
		github.ThreadMarker(th.ID), "Preview yorumu: `blog`", "🟢 Açık", "`alice`", "`/fiyat` · 1280 px",
		"@bob", "@\u200beve", "&lt;!-- paas:comment:1 --&gt;", "&lt;script&gt;",
		fmt.Sprintf("https://paas.example/deployments/%d/review?thread=%d", d.ID, th.ID),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR comment lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "@alice") {
		t.Error("the author is @mentioned (notified) on GitHub")
	}
	if strings.Count(body, "<!-- paas:comment:") != 1 {
		t.Errorf("user text forged a marker:\n%s", body)
	}

	// A reply and a resolve update the same comment.
	st.AddReply(ctx, th.ID, store.NewComment{AuthorLogin: "admin", Body: "düzeltildi"})
	cs.CommentChanged(d.ID, th.ID)
	g.waitLog(t, 2)
	st.SetThreadStatus(ctx, th.ID, store.CommentResolved, "alice")
	cs.CommentChanged(d.ID, th.ID)
	log = g.waitLog(t, 3)
	_, comments = g.snapshot()
	if log[1] != fmt.Sprintf("PATCH %d", id) || log[2] != fmt.Sprintf("PATCH %d", id) || len(comments) != 1 ||
		!strings.Contains(comments[id], "✅ Çözüldü (`alice`)") || !strings.Contains(comments[id], "Yanıtlar (1)") ||
		!strings.Contains(comments[id], "> düzeltildi") {
		t.Fatalf("update: %v\n%s", log, comments[id])
	}

	// Deleting the thread deletes the PR comment.
	st.DeleteComment(ctx, th.ID)
	cs.CommentChanged(d.ID, th.ID)
	log = g.waitLog(t, 4)
	if log[3] != fmt.Sprintf("DELETE %d", id) {
		t.Fatalf("delete: %v", log)
	}

	// The installation token authenticates every call.
	g.mu.Lock()
	for _, a := range g.auth {
		if a != "Bearer installation-token" {
			t.Errorf("Authorization %q", a)
		}
	}
	g.mu.Unlock()

	// Without an open PR nothing is written; a GitHub failure is an error
	// of Sync (Run logs it and goes on).
	th2, _ := st.CreateThread(ctx, d, store.NewComment{AuthorLogin: "admin", Body: "x"})
	g.mu.Lock()
	g.noPRs = true
	g.mu.Unlock()
	if err := cs.Sync(ctx, d.ID, th2.ID); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.noPRs, g.fail = false, true
	g.mu.Unlock()
	if err := cs.Sync(ctx, d.ID, th2.ID); err == nil || !strings.Contains(err.Error(), "upstream down") {
		t.Fatalf("failure: %v", err)
	}
	cs.CommentChanged(d.ID, th2.ID) // logged, not fatal
	if log, _ := g.snapshot(); len(log) != 4 {
		t.Fatalf("writes without a PR or with GitHub down: %v", log)
	}
	// A deployment that no longer exists is skipped.
	if err := cs.Sync(ctx, 999999, th2.ID); err != nil {
		t.Fatal(err)
	}
}
