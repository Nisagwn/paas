package cleanup_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/cleanup"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

// fakeRetirer records which deployments had their cluster objects deleted.
type fakeRetirer struct {
	mu      sync.Mutex
	retired []int64
	failFor map[int64]bool
}

func (f *fakeRetirer) Retire(_ context.Context, d store.Deployment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFor[d.ID] {
		return errors.New("kubernetes API unavailable")
	}
	f.retired = append(f.retired, d.ID)
	return nil
}

func (f *fakeRetirer) ids() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]int64(nil), f.retired...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

type countingRouter struct{ n int }

func (r *countingRouter) SyncApp(context.Context, string) error { r.n++; return nil }

func sha(n int) string { return fmt.Sprintf("%040x", n) }

func TestCollectApp(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")

	ready := func(n int, branch string, aliases ...store.AliasSpec) store.Deployment {
		d, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(n), branch, "")
		if err := st.MarkReady(ctx, d, aliases); err != nil {
			t.Fatal(err)
		}
		return d
	}
	prod := store.AliasSpec{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}
	preview := store.AliasSpec{Hostname: "feature-blog.paas.test", Kind: store.AliasPreview, Branch: "feature"}

	// Four production deployments; production points at the newest.
	m1, m2 := ready(1, "main", prod), ready(2, "main", prod)
	m3, m4 := ready(3, "main", prod), ready(4, "main", prod)
	// Two previews of one branch: only the newest keeps the alias.
	f1, f2 := ready(5, "feature", preview), ready(6, "feature", preview)
	// A failed deployment may have left objects in the cluster.
	failed, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(7), "main", "")
	st.MarkFailed(ctx, failed.ID, "CrashLoopBackOff")

	// Generous margins: finished_at and the TTL check use the database clock,
	// which inside Docker Desktop can be adjusted by a few milliseconds.
	time.Sleep(time.Second) // let the preview TTL below pass
	ret := &fakeRetirer{failFor: map[int64]bool{m1.ID: true}}
	router := &countingRouter{}
	c := cleanup.New(st, ret, router, cleanup.Policy{KeepProduction: 2, PreviewTTL: 200 * time.Millisecond},
		time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r := c.CollectApp(ctx, app, false)
	// m1, m2 are beyond the newest two production deployments; f1 lost its alias.
	if r.Retired != 3 {
		t.Fatalf("retired %d, want 3 (result %+v)", r.Retired, r)
	}
	// m1's deletion failed, so it is not marked cleaned; the failed deploy is.
	want := []int64{m2.ID, f1.ID, failed.ID}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if got := ret.ids(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("objects deleted for %v, want %v", got, want)
	}
	if r.Cleaned != 3 || r.Errors != 1 {
		t.Fatalf("result %+v, want 3 cleaned and 1 error", r)
	}

	for _, keep := range []store.Deployment{m3, m4, f2} {
		if d, _ := st.GetDeployment(ctx, keep.ID); d.Status != store.StatusReady {
			t.Errorf("deployment %d (%s) is %s, must be kept", keep.ID, keep.Branch, d.Status)
		}
	}
	if d, _ := st.GetDeployment(ctx, m1.ID); d.Status != store.StatusRetired {
		t.Errorf("m1 is %s, want retired even though its objects are not gone yet", d.Status)
	}
	if aliases, _ := st.ListAliases(ctx, app.ID); len(aliases) != 2 {
		t.Errorf("aliases = %+v, cleanup must not touch them", aliases)
	}
	if router.n != 0 {
		t.Errorf("router synced %d times without alias changes", router.n)
	}

	// Next sweep: the API is back, m1's objects are deleted, nothing else changes.
	ret.failFor = nil
	r = c.CollectApp(ctx, app, true)
	if r.Retired != 0 || r.Cleaned != 1 || r.Errors != 0 {
		t.Fatalf("second sweep %+v, want only m1 cleaned", r)
	}
	if router.n != 1 {
		t.Errorf("a forced sweep must sync routes once, got %d", router.n)
	}
	// Retired deployments can never take production traffic again.
	if _, err := st.Rollback(ctx, app.ID, m1.ID); !errors.Is(err, store.ErrRetired) {
		t.Fatalf("rollback to a retired deployment: %v, want ErrRetired", err)
	}
}
