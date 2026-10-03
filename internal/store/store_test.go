package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/nisagwn/minipaas/internal/store"
	"github.com/nisagwn/minipaas/internal/testdb"
)

func sha(n int) string { return fmt.Sprintf("%040x", n) }

// Many workers polling at once must never claim the same deployment twice.
func TestClaimNextIsExclusive(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()

	app, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	const jobs = 50
	for i := 1; i <= jobs; i++ {
		if _, _, err := st.EnqueueDeployment(ctx, app.ID, sha(i), "main", ""); err != nil {
			t.Fatal(err)
		}
	}

	var (
		mu      sync.Mutex
		claimed = map[int64]int{}
		wg      sync.WaitGroup
	)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				d, err := st.ClaimNext(ctx)
				if errors.Is(err, store.ErrNotFound) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				claimed[d.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != jobs {
		t.Fatalf("claimed %d distinct deployments, want %d", len(claimed), jobs)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("deployment %d claimed %d times", id, n)
		}
	}
}

func TestEnqueueIsIdempotent(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")

	d1, created1, err := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "first")
	if err != nil || !created1 {
		t.Fatalf("first enqueue: created=%v err=%v", created1, err)
	}
	d2, created2, err := st.EnqueueDeployment(ctx, app.ID, sha(1), "feature", "again")
	if err != nil || created2 || d2.ID != d1.ID {
		t.Fatalf("second enqueue: created=%v id=%d (want %d) err=%v", created2, d2.ID, d1.ID, err)
	}
}

// An older commit that finishes after a newer one must not take the alias back.
func TestAliasOnlyMovesForward(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	older, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "")
	newer, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(2), "main", "")
	prod := []store.AliasSpec{{Hostname: "blog.test", Kind: store.AliasProduction, Branch: "main"}}

	if err := st.MarkReady(ctx, newer, prod); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkReady(ctx, older, prod); err != nil {
		t.Fatal(err)
	}
	aliases, _ := st.ListAliases(ctx, app.ID)
	if len(aliases) != 1 || aliases[0].DeploymentID != newer.ID {
		t.Fatalf("aliases = %+v, want production -> %d", aliases, newer.ID)
	}
}

// AliasRoutes follows a rollback: the router reads it to point hostnames at commits.
func TestAliasRoutes(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	first, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "")
	second, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(2), "main", "")
	prod := store.AliasSpec{Hostname: "blog.test", Kind: store.AliasProduction, Branch: "main"}
	preview := store.AliasSpec{Hostname: "main-blog.test", Kind: store.AliasPreview, Branch: "main"}
	st.MarkReady(ctx, first, []store.AliasSpec{prod, preview})
	st.MarkReady(ctx, second, []store.AliasSpec{prod, preview})

	if _, err := st.Rollback(ctx, app.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	routes, err := st.AliasRoutes(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.AliasRoute{
		{Hostname: "blog.test", Kind: store.AliasProduction, Branch: "main", DeploymentID: first.ID, CommitSHA: sha(1)},
		{Hostname: "main-blog.test", Kind: store.AliasPreview, Branch: "main", DeploymentID: second.ID, CommitSHA: sha(2)},
	}
	if fmt.Sprint(routes) != fmt.Sprint(want) {
		t.Fatalf("routes = %+v\nwant     %+v", routes, want)
	}
}

func TestDuplicateApp(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	if _, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateApp(ctx, "blog", "other/repo", "main"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
}

func TestAppEnv(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	other, _ := st.CreateApp(ctx, "shop", "nisagwn/shop", "main")
	str := func(s string) *string { return &s }

	if err := st.UpdateAppEnv(ctx, app.ID, map[string]*string{"A": str("1"), "B": str("2")}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAppEnv(ctx, other.ID, map[string]*string{"A": str("other")}); err != nil {
		t.Fatal(err)
	}
	// Update one, delete one, leave the rest; deleting a missing key is fine.
	if err := st.UpdateAppEnv(ctx, app.ID, map[string]*string{"A": str("x"), "B": nil, "C": str(""), "NOPE": nil}); err != nil {
		t.Fatal(err)
	}
	env, err := st.AppEnv(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 2 || env["A"] != "x" || env["C"] != "" {
		t.Fatalf("env = %v", env)
	}
	if env, _ := st.AppEnv(ctx, other.ID); len(env) != 1 || env["A"] != "other" {
		t.Fatalf("other app env = %v", env)
	}
	// The schema rejects keys that cannot be environment variable names.
	if err := st.UpdateAppEnv(ctx, app.ID, map[string]*string{"1BAD": str("v")}); err == nil {
		t.Fatal("invalid key accepted")
	}
}
