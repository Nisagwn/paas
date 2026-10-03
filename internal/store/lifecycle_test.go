package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

// deploy runs one commit through the queue to ready, with the aliases the
// worker would give it.
func deploy(t *testing.T, st *store.Store, app store.App, n int, branch string) store.Deployment {
	t.Helper()
	ctx := context.Background()
	if _, _, err := st.EnqueueDeployment(ctx, app.ID, sha(n), branch, ""); err != nil {
		t.Fatal(err)
	}
	d, err := st.ClaimNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	aliases := []store.AliasSpec{{Hostname: branch + "-" + app.Name + ".paas.test", Kind: store.AliasPreview, Branch: branch}}
	if branch == app.ProductionBranch {
		aliases = append(aliases, store.AliasSpec{Hostname: app.Name + ".paas.test", Kind: store.AliasProduction, Branch: branch})
	}
	if err := st.MarkReady(ctx, d, aliases); err != nil {
		t.Fatal(err)
	}
	return d
}

func ids(ds []store.Deployment) []int64 {
	out := []int64{}
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

func TestRetirementPolicy(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")

	var main []store.Deployment
	for i := 1; i <= 6; i++ {
		main = append(main, deploy(t, st, app, i, "main"))
	}
	f1 := deploy(t, st, app, 10, "feature")
	f2 := deploy(t, st, app, 11, "feature") // takes the preview alias from f1

	// Roll production back to main[1]: an alias pins it.
	if _, err := st.Rollback(ctx, app.ID, main[1].ID); err != nil {
		t.Fatal(err)
	}

	// Keep the newest 3 production deployments; previews never expire yet.
	got, err := st.RetireCandidates(ctx, app.ID, 3, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// main[5] has the main preview alias, main[1] production, main[3..5] are
	// the newest three: left are main[0] and main[2].
	if want := []int64{main[0].ID, main[2].ID}; !slices.Equal(ids(got), want) {
		t.Fatalf("candidates = %v, want %v", ids(got), want)
	}

	time.Sleep(30 * time.Millisecond)
	got, _ = st.RetireCandidates(ctx, app.ID, 3, 10*time.Millisecond)
	if want := []int64{main[0].ID, main[2].ID, f1.ID}; !slices.Equal(ids(got), want) {
		t.Fatalf("candidates with preview TTL = %v, want %v (f2 is aliased)", ids(got), want)
	}

	for _, d := range got {
		if ok, err := st.Retire(ctx, d.ID, "test"); err != nil || !ok {
			t.Fatalf("retire %d: %v %v", d.ID, ok, err)
		}
	}
	// Aliased deployments are never retired, even when asked directly.
	for _, d := range []store.Deployment{main[1], f2} {
		if ok, err := st.Retire(ctx, d.ID, "test"); err != nil || ok {
			t.Fatalf("retire aliased %d: %v %v", d.ID, ok, err)
		}
	}
	// Retiring twice is a no-op.
	if ok, _ := st.Retire(ctx, main[0].ID, "again"); ok {
		t.Fatal("retired twice")
	}

	r, _ := st.GetDeployment(ctx, main[0].ID)
	if r.Status != store.StatusRetired || r.RetiredAt == nil || r.RetireReason != "test" {
		t.Fatalf("retired deployment: %+v", r)
	}
	if _, err := st.Rollback(ctx, app.ID, main[0].ID); !errors.Is(err, store.ErrRetired) {
		t.Fatalf("rollback to retired: %v, want ErrRetired", err)
	}

	// Pending cleanup: retired ones until MarkCleaned.
	pending, _ := st.PendingCleanup(ctx, app.ID, 100)
	if len(pending) != 3 {
		t.Fatalf("pending = %v, want 3", ids(pending))
	}
	for _, d := range pending {
		if err := st.MarkCleaned(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	if pending, _ = st.PendingCleanup(ctx, app.ID, 100); len(pending) != 0 {
		t.Fatalf("pending after cleanup = %v", ids(pending))
	}

	// Pushing a retired commit again queues it again (e.g. a revert).
	d, created, err := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "revert")
	if err != nil || !created || d.ID != main[0].ID || d.Status != store.StatusQueued || d.RetiredAt != nil {
		t.Fatalf("re-push of retired commit: created=%v %+v %v", created, d, err)
	}
}

func TestDeleteBranch(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")

	deploy(t, st, app, 1, "main")
	old := deploy(t, st, app, 2, "feature")
	head := deploy(t, st, app, 3, "feature")
	// Production was rolled back to a preview deployment: it must survive.
	if _, err := st.Rollback(ctx, app.ID, old.ID); err != nil {
		t.Fatal(err)
	}
	st.EnqueueDeployment(ctx, app.ID, sha(4), "feature", "")
	building, _ := st.ClaimNext(ctx)
	queued, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(5), "feature", "")

	bc, err := st.DeleteBranch(ctx, app.ID, "feature", "branch deleted")
	if err != nil {
		t.Fatal(err)
	}
	want := store.BranchCleanup{AliasesRemoved: 1, Retired: 1, Cancelled: 1, InFlight: 1}
	if bc != want {
		t.Fatalf("got %+v, want %+v", bc, want)
	}

	status := func(id int64) string {
		d, _ := st.GetDeployment(ctx, id)
		return d.Status
	}
	if status(head.ID) != store.StatusRetired || status(old.ID) != store.StatusReady || status(queued.ID) != store.StatusRetired {
		t.Fatalf("statuses: head=%s old=%s queued=%s", status(head.ID), status(old.ID), status(queued.ID))
	}
	aliases, _ := st.ListAliases(ctx, app.ID)
	for _, a := range aliases {
		if a.Branch == "feature" {
			t.Fatalf("preview alias left: %+v", a)
		}
	}

	// The in-flight deployment finishes as retired, without aliases.
	err = st.MarkReady(ctx, building, []store.AliasSpec{{Hostname: "feature-blog.paas.test", Kind: store.AliasPreview, Branch: "feature"}})
	if !errors.Is(err, store.ErrRetired) || status(building.ID) != store.StatusRetired {
		t.Fatalf("MarkReady after branch deletion: %v, status %s", err, status(building.ID))
	}
	aliases, _ = st.ListAliases(ctx, app.ID)
	for _, a := range aliases {
		if a.Branch == "feature" {
			t.Fatalf("alias recreated for deleted branch: %+v", a)
		}
	}

	// Cleanup: the retired ones that ran; the cancelled one created nothing.
	pending, _ := st.PendingCleanup(ctx, app.ID, 100)
	if want := []int64{head.ID, building.ID}; !slices.Equal(ids(pending), want) {
		t.Fatalf("pending = %v, want %v", ids(pending), want)
	}
}

func TestExpirePreviewAliases(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	deploy(t, st, app, 1, "main")
	deploy(t, st, app, 2, "feature")

	if n, _ := st.ExpirePreviewAliases(ctx, app.ID, time.Hour); n != 0 {
		t.Fatalf("expired %d fresh aliases", n)
	}
	time.Sleep(30 * time.Millisecond)
	// Only the feature preview goes; production and main's preview stay.
	if n, err := st.ExpirePreviewAliases(ctx, app.ID, 10*time.Millisecond); err != nil || n != 1 {
		t.Fatalf("expired %d (%v), want 1", n, err)
	}
	aliases, _ := st.ListAliases(ctx, app.ID)
	if len(aliases) != 2 {
		t.Fatalf("aliases left: %+v", aliases)
	}
}

func TestRecoverStale(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "")

	d, _ := st.ClaimNext(ctx) // attempt 1, then the worker "dies"
	if d.Attempts != 1 {
		t.Fatalf("attempts = %d", d.Attempts)
	}
	// A live heartbeat is not stale.
	if rec, _ := st.RecoverStale(ctx, time.Hour, 2, "gone"); len(rec) != 0 {
		t.Fatalf("recovered a live deployment: %+v", rec)
	}
	time.Sleep(30 * time.Millisecond)
	rec, err := st.RecoverStale(ctx, 10*time.Millisecond, 2, "gone")
	if err != nil || len(rec) != 1 || rec[0].Status != store.StatusQueued || rec[0].AppName != "blog" {
		t.Fatalf("first recovery: %+v %v", rec, err)
	}
	// The dead attempt's heartbeat must not count any more.
	if ok, _ := st.Heartbeat(ctx, d.ID, 1); ok {
		t.Fatal("heartbeat of a recovered attempt accepted")
	}

	d, _ = st.ClaimNext(ctx) // attempt 2
	if ok, _ := st.Heartbeat(ctx, d.ID, 2); !ok {
		t.Fatal("heartbeat of the current attempt rejected")
	}
	time.Sleep(30 * time.Millisecond)
	rec, _ = st.RecoverStale(ctx, 10*time.Millisecond, 2, "worker restarted")
	if len(rec) != 1 || rec[0].Status != store.StatusFailed {
		t.Fatalf("second recovery: %+v, want failed", rec)
	}
	got, _ := st.GetDeployment(ctx, d.ID)
	if got.Status != store.StatusFailed || got.Error != "worker restarted" || got.FinishedAt == nil {
		t.Fatalf("after giving up: %+v", got)
	}
}
