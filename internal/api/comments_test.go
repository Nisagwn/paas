package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/store"
)

// recordSync is an api.CommentSync that records the changed threads.
type recordSync struct {
	mu      sync.Mutex
	changed []string
}

func (r *recordSync) CommentChanged(deploymentID, threadID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changed = append(r.changed, fmt.Sprintf("%d/%d", deploymentID, threadID))
}

func (r *recordSync) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.changed
	r.changed = nil
	return out
}

// withCommentSync restarts the fixture's server with a CommentSync.
func withCommentSync(f *teamFixture) *recordSync {
	rec := &recordSync{}
	f.srv.Close()
	f.srv = httptest.NewServer((&api.Server{
		Store: f.st, Domain: domain, APIToken: token, WebhookSecret: secret,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Sessions: f.ss, Comments: rec,
	}).Handler())
	f.t.Cleanup(f.srv.Close)
	return rec
}

func decode[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return v
}

func TestCommentValidation(t *testing.T) {
	f := setupTeams(t)
	rec := withCommentSync(f)
	ctx := context.Background()
	path := fmt.Sprintf("/api/deployments/%d/comments", f.d1.ID)

	for _, c := range []struct {
		name string
		body any
		want int
		msg  string
	}{
		{"empty", map[string]any{"body": "  "}, 400, "comment body is required"},
		{"long", map[string]any{"body": strings.Repeat("ç", store.MaxCommentBody+1)}, 400, "longer than 2000"},
		{"control", map[string]any{"body": "a\x00b"}, 400, "control characters"},
		{"relative path", map[string]any{"body": "x", "path": "pricing"}, 400, "must start with a single"},
		{"protocol-relative path", map[string]any{"body": "x", "path": "//evil.example"}, 400, "must start with a single"},
		{"spaces", map[string]any{"body": "x", "path": "/a b"}, 400, "spaces"},
		{"x without y", map[string]any{"body": "x", "x": 10}, 400, "together"},
		{"x over 100", map[string]any{"body": "x", "x": 100.5, "y": 1}, 400, "between 0 and 100"},
		{"negative y", map[string]any{"body": "x", "x": 1, "y": -1}, 400, "between 0 and 100"},
		{"viewport", map[string]any{"body": "x", "viewport_width": 0}, 400, "viewport_width"},
		{"unknown field", map[string]any{"body": "x", "status": "resolved"}, 400, "unknown field"},
	} {
		if code, b := f.call("carol", "POST", path, c.body); code != c.want || !strings.Contains(b, c.msg) {
			t.Errorf("%s: %d %s", c.name, code, b)
		}
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("rejected comments were synced: %v", got)
	}

	// A valid thread: mentions resolve to team members only, canonical case.
	code, b := f.call("carol", "POST", path, map[string]any{
		"body": "@BOB bak, @dave ekipte değil, mail@example.com da değil", "path": "/fiyat?plan=pro", "x": 33.3, "y": 50, "viewport_width": 390,
	})
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	th := decode[store.Comment](t, b)
	if th.AuthorLogin != "carol" || th.Path != "/fiyat?plan=pro" || *th.X != 33.3 || *th.ViewportWidth != 390 ||
		strings.Join(th.Mentions, ",") != "bob" || th.Status != "open" {
		t.Fatalf("thread: %+v", th)
	}
	if got := rec.take(); len(got) != 1 || got[0] != fmt.Sprintf("%d/%d", f.d1.ID, th.ID) {
		t.Fatalf("sync after create: %v", got)
	}

	// Replies: to the thread, or to a reply (lands on its thread).
	code, b = f.call("bob", "POST", fmt.Sprintf("/api/comments/%d/replies", th.ID), map[string]string{"body": "tamam @carol"})
	reply := decode[store.Comment](t, b)
	if code != 201 || reply.ThreadID == nil || *reply.ThreadID != th.ID || strings.Join(reply.Mentions, ",") != "carol" {
		t.Fatalf("reply: %d %s", code, b)
	}
	code, b = f.call("alice", "POST", fmt.Sprintf("/api/comments/%d/replies", reply.ID), map[string]string{"body": "ek"})
	if r2 := decode[store.Comment](t, b); code != 201 || *r2.ThreadID != th.ID {
		t.Fatalf("reply to reply: %d %s", code, b)
	}
	if code, _ := f.call("alice", "POST", fmt.Sprintf("/api/comments/%d/replies", th.ID), map[string]string{"body": ""}); code != 400 {
		t.Fatalf("empty reply: %d", code)
	}
	if got := rec.take(); len(got) != 2 {
		t.Fatalf("sync after replies: %v", got)
	}

	// Replies cannot be resolved; the viewer resolves her own thread but
	// not alice's.
	if code, _ := f.call("bob", "POST", fmt.Sprintf("/api/comments/%d/resolve", reply.ID), nil); code != 400 {
		t.Fatalf("resolve reply: %d", code)
	}
	if code, b := f.call("carol", "POST", fmt.Sprintf("/api/comments/%d/resolve", th.ID), nil); code != 200 ||
		decode[store.Comment](t, b).ResolvedBy != "carol" {
		t.Fatalf("author resolves: %d %s", code, b)
	}
	aliceID := f.users["alice"].ID
	other, _ := f.st.CreateThread(ctx, f.d1, store.NewComment{AuthorID: &aliceID, AuthorLogin: "alice", Body: "x"})
	if code, _ := f.call("carol", "POST", fmt.Sprintf("/api/comments/%d/resolve", other.ID), nil); code != 403 {
		t.Fatalf("viewer resolves another's thread: %d", code)
	}

	// Listing per deployment and per app, with the status filter.
	code, b = f.call("carol", "GET", path+"?status=resolved", nil)
	list := decode[struct {
		Counts  store.CommentCount `json:"counts"`
		Threads []store.Comment    `json:"threads"`
	}](t, b)
	if code != 200 || len(list.Threads) != 1 || len(list.Threads[0].Replies) != 2 || list.Counts.Open != 1 || list.Counts.Resolved != 1 {
		t.Fatalf("list: %d %s", code, b)
	}
	if code, _ := f.call("carol", "GET", path+"?status=nope", nil); code != 400 {
		t.Fatalf("bad filter: %d", code)
	}
	if code, b := f.call("carol", "GET", "/api/apps/blog/comments?status=open&limit=5", nil); code != 200 || !strings.Contains(b, `"open":1`) {
		t.Fatalf("app list: %d %s", code, b)
	}
	if code, _ := f.call("carol", "GET", "/api/apps/blog/comments?limit=0", nil); code != 400 {
		t.Fatalf("bad limit: %d", code)
	}

	// Only ready deployments take new threads.
	blog, _ := f.st.GetAppByName(ctx, "blog")
	queued, _, _ := f.st.EnqueueDeployment(ctx, blog.ID, sha(9), "feature", "wip")
	if code, b := f.call("bob", "POST", fmt.Sprintf("/api/deployments/%d/comments", queued.ID), map[string]string{"body": "x"}); code != 409 {
		t.Fatalf("queued deployment: %d %s", code, b)
	}
}

// Authors delete their own comments, owners moderate; nobody else can.
func TestCommentDelete(t *testing.T) {
	f := setupTeams(t)
	rec := withCommentSync(f)
	ctx := context.Background()
	newThread := func(who string) store.Comment {
		var id *int64
		if u, ok := f.users[who]; ok {
			id = &u.ID
		}
		c, err := f.st.CreateThread(ctx, f.d1, store.NewComment{AuthorID: id, AuthorLogin: who, Body: "by " + who})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	del := func(who string, c store.Comment) int {
		code, _ := f.call(who, "DELETE", fmt.Sprintf("/api/comments/%d", c.ID), nil)
		return code
	}
	carols := newThread("carol")
	for who, want := range map[string]int{"anonymous": 401, "bob": 403, "dave": 404} {
		if got := del(who, carols); got != want {
			t.Errorf("%s deletes carol's thread: %d, want %d", who, got, want)
		}
	}
	if got := del("carol", carols); got != 204 {
		t.Fatalf("author deletes: %d", got)
	}
	if got := del("carol", carols); got != 404 {
		t.Fatalf("deleted twice: %d", got)
	}
	// Bob (member) cannot delete alice's; alice (owner) deletes bob's.
	if got := del("bob", newThread("alice")); got != 403 {
		t.Fatalf("member deletes another's: %d", got)
	}
	if got := del("alice", newThread("bob")); got != 204 {
		t.Fatalf("owner moderates: %d", got)
	}
	// The admin token wrote this one (no user).
	if got := del("admin", newThread("admin")); got != 204 {
		t.Fatalf("admin deletes own: %d", got)
	}
	// Deleting a reply syncs its thread.
	th := newThread("bob")
	carolID := f.users["carol"].ID
	reply, _ := f.st.AddReply(ctx, th.ID, store.NewComment{AuthorID: &carolID, AuthorLogin: "carol", Body: "r"})
	rec.take()
	if got := del("carol", reply); got != 204 {
		t.Fatalf("delete reply: %d", got)
	}
	if got := rec.take(); len(got) != 1 || got[0] != fmt.Sprintf("%d/%d", f.d1.ID, th.ID) {
		t.Fatalf("sync after reply delete: %v", got)
	}
	if c, _ := f.st.GetComment(ctx, th.ID); len(c.Replies) != 0 {
		t.Fatal("reply not deleted")
	}
}

func TestMentionedLogins(t *testing.T) {
	got := api.MentionedLogins("@alice ve @Bob, @alice tekrar; mail@x.com @-bad (@carol-2) /@path")
	if strings.Join(got, ",") != "alice,Bob,carol-2" {
		t.Fatalf("mentions: %v", got)
	}
}
