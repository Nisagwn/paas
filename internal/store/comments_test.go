package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

// Faz 25: threads, replies, status, counts and constraints.
func TestPreviewComments(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	alice, _ := st.UpsertUser(ctx, 1, "alice", "", "")
	team, _ := st.CreateTeam(ctx, "web", "Web", alice.ID)
	app, err := st.CreateAppInTeam(ctx, team.ID, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := st.CreateAppInTeam(ctx, team.ID, "shop", "nisagwn/shop", "main")
	sha := func(n int) string { return fmt.Sprintf("%040x", n) }
	d1, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "feature", "one")
	d2, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(2), "feature", "two")
	d3, _, _ := st.EnqueueDeployment(ctx, other.ID, sha(3), "main", "three")

	x, y, vw := 12.5, 80.0, 1280
	uid := alice.ID
	t1, err := st.CreateThread(ctx, d1, store.NewComment{AuthorID: &uid, AuthorLogin: "alice", Body: "Logo kaymış @bob",
		Mentions: []string{"bob"}, Path: "/pricing", X: &x, Y: &y, ViewportWidth: &vw})
	if err != nil {
		t.Fatal(err)
	}
	if t1.Status != store.CommentOpen || t1.Path != "/pricing" || *t1.X != 12.5 || *t1.ViewportWidth != 1280 ||
		t1.AppID != app.ID || !t1.IsThread() || len(t1.Mentions) != 1 {
		t.Fatalf("thread: %+v", t1)
	}
	// Unpinned thread by the admin (no user), default path.
	t2, err := st.CreateThread(ctx, d1, store.NewComment{AuthorLogin: "admin", Body: "Genel not"})
	if err != nil || t2.Path != "/" || t2.X != nil || t2.AuthorID != nil || t2.Mentions == nil {
		t.Fatalf("unpinned: %+v %v", t2, err)
	}
	st.CreateThread(ctx, d2, store.NewComment{AuthorLogin: "admin", Body: "d2"})
	st.CreateThread(ctx, d3, store.NewComment{AuthorLogin: "admin", Body: "other app"})

	r1, err := st.AddReply(ctx, t1.ID, store.NewComment{AuthorID: &uid, AuthorLogin: "alice", Body: "düzeltildi"})
	if err != nil || r1.ThreadID == nil || *r1.ThreadID != t1.ID || r1.Status != "" || r1.DeploymentID != d1.ID {
		t.Fatalf("reply: %+v %v", r1, err)
	}
	if _, err := st.AddReply(ctx, r1.ID, store.NewComment{AuthorLogin: "admin", Body: "x"}); !errors.Is(err, store.ErrNotThread) {
		t.Fatalf("reply to a reply: %v", err)
	}
	if _, err := st.AddReply(ctx, 999999, store.NewComment{AuthorLogin: "admin", Body: "x"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reply to nothing: %v", err)
	}

	threads, err := st.ListThreads(ctx, d1.ID, "")
	if err != nil || len(threads) != 2 || threads[0].ID != t1.ID || len(threads[0].Replies) != 1 || len(threads[1].Replies) != 0 {
		t.Fatalf("list: %+v %v", threads, err)
	}
	got, err := st.GetComment(ctx, t1.ID)
	if err != nil || len(got.Replies) != 1 || got.Replies[0].Body != "düzeltildi" {
		t.Fatalf("get: %+v %v", got, err)
	}

	// Resolve and reopen; replies cannot be resolved.
	res, err := st.SetThreadStatus(ctx, t1.ID, store.CommentResolved, "bob")
	if err != nil || res.Status != store.CommentResolved || res.ResolvedBy != "bob" || res.ResolvedAt == nil || len(res.Replies) != 1 {
		t.Fatalf("resolve: %+v %v", res, err)
	}
	if _, err := st.SetThreadStatus(ctx, r1.ID, store.CommentResolved, "bob"); !errors.Is(err, store.ErrNotThread) {
		t.Fatalf("resolve reply: %v", err)
	}
	if _, err := st.SetThreadStatus(ctx, 999999, store.CommentResolved, "bob"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolve nothing: %v", err)
	}
	if open, _ := st.ListThreads(ctx, d1.ID, store.CommentOpen); len(open) != 1 || open[0].ID != t2.ID {
		t.Fatalf("open filter: %+v", open)
	}

	// Counts per deployment and per app.
	if c, _ := st.CountThreads(ctx, d1.ID); c.Open != 1 || c.Resolved != 1 {
		t.Fatalf("deployment counts: %+v", c)
	}
	if c, _ := st.CountAppThreads(ctx, app.ID); c.Open != 2 || c.Resolved != 1 {
		t.Fatalf("app counts: %+v", c)
	}
	counts, err := st.OpenCommentCounts(ctx, app.ID)
	if err != nil || counts[d1.ID] != 1 || counts[d2.ID] != 1 || counts[d3.ID] != 0 {
		t.Fatalf("open counts: %v %v", counts, err)
	}
	appThreads, _ := st.ListAppThreads(ctx, app.ID, "", 10)
	if len(appThreads) != 3 || appThreads[0].DeploymentID != d2.ID {
		t.Fatalf("app threads (newest first): %+v", appThreads)
	}

	reopened, err := st.SetThreadStatus(ctx, t1.ID, store.CommentOpen, "alice")
	if err != nil || reopened.Status != store.CommentOpen || reopened.ResolvedAt != nil || reopened.ResolvedBy != "" {
		t.Fatalf("reopen: %+v %v", reopened, err)
	}

	// Deleting a thread deletes its replies; deleting twice is ErrNotFound.
	if err := st.DeleteComment(ctx, t1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetComment(ctx, r1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reply survived its thread: %v", err)
	}
	if err := st.DeleteComment(ctx, t1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}

	// The schema enforces the limits too.
	long := strings.Repeat("a", store.MaxCommentBody+1)
	if _, err := st.CreateThread(ctx, d1, store.NewComment{AuthorLogin: "admin", Body: long}); err == nil {
		t.Fatal("body over the limit stored")
	}
	bad := 101.0
	if _, err := st.CreateThread(ctx, d1, store.NewComment{AuthorLogin: "admin", Body: "x", X: &bad, Y: &y}); err == nil {
		t.Fatal("x over 100 stored")
	}
	if _, err := st.CreateThread(ctx, d1, store.NewComment{AuthorLogin: "admin", Body: "x", Path: "pricing"}); err == nil {
		t.Fatal("relative path stored")
	}
}
