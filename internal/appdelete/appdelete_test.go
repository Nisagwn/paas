package appdelete_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nisagwn/paas/internal/appdelete"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

// cluster fails once, then reports a terminating namespace once, then done.
type cluster struct {
	calls int
}

func (c *cluster) DeleteApp(context.Context, string, int64) (bool, error) {
	c.calls++
	switch c.calls {
	case 1:
		return false, errors.New("api server unreachable")
	case 2:
		return false, nil
	}
	return true, nil
}

// A deletion survives cluster errors and a terminating namespace: every
// sweep repeats the remaining steps until the namespace is gone; only then
// is the name free. A wrong confirmation changes nothing.
func TestDeletionResumes(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	c := &cluster{}
	svc := &appdelete.Service{Store: st, Cluster: c}
	if _, err := svc.Delete(ctx, app, "blgo", store.DeletionActor{Login: "admin"}); !errors.Is(err, appdelete.ErrConfirm) {
		t.Fatalf("wrong confirmation: %v", err)
	}
	if _, err := st.GetAppByName(ctx, "blog"); err != nil {
		t.Fatalf("app after a refused deletion: %v", err)
	}

	res, err := svc.Delete(ctx, app, "blog", store.DeletionActor{Login: "admin"})
	if err != nil || res.Done {
		t.Fatalf("first run: %+v %v", res, err)
	}
	// Rows are gone even though the cluster step failed.
	if _, err := st.GetAppByID(ctx, app.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("app row: %v", err)
	}
	d, _ := st.GetAppDeletion(ctx, res.Deletion.ID)
	if d.Attempts != 1 || d.LastError == "" || d.FinishedAt != nil {
		t.Fatalf("after the failure: %+v", d)
	}
	if _, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main"); !errors.Is(err, store.ErrAppDeleting) {
		t.Fatalf("name while the namespace exists: %v", err)
	}
	svc.Sweep(ctx) // terminating
	if d, _ := st.GetAppDeletion(ctx, d.ID); d.FinishedAt != nil || d.LastError != "" {
		t.Fatalf("while terminating: %+v", d)
	}
	svc.Sweep(ctx) // gone
	if d, _ := st.GetAppDeletion(ctx, d.ID); d.FinishedAt == nil {
		t.Fatalf("not finished: %+v", d)
	}
	svc.Sweep(ctx) // nothing pending: no more cluster calls
	if c.calls != 3 {
		t.Fatalf("cluster calls: %d", c.calls)
	}
	if _, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main"); err != nil {
		t.Fatalf("name after the deletion: %v", err)
	}
}

// images fails once, then deletes.
type images struct {
	calls int
	apps  []string
}

func (i *images) DeleteAppImages(_ context.Context, app string) (int, error) {
	i.calls++
	if i.calls == 1 {
		return 0, errors.New("registry unavailable")
	}
	i.apps = append(i.apps, app)
	return 3, nil
}

// Faz 30: the project's images leave the registry before the name is
// free; a registry failure is retried like a cluster failure.
func TestDeletionImages(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	img := &images{}
	svc := &appdelete.Service{Store: st, Images: img}
	res, err := svc.Delete(ctx, app, "blog", store.DeletionActor{Login: "admin"})
	if err != nil || res.Done {
		t.Fatalf("first run: %+v %v", res, err)
	}
	if _, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main"); !errors.Is(err, store.ErrAppDeleting) {
		t.Fatalf("name while images remain: %v", err)
	}
	svc.Sweep(ctx)
	if d, _ := st.GetAppDeletion(ctx, res.Deletion.ID); d.FinishedAt == nil {
		t.Fatalf("not finished: %+v", d)
	}
	if len(img.apps) != 1 || img.apps[0] != "blog" {
		t.Fatalf("images deleted for %v", img.apps)
	}
}

// Without a cluster (dry-run deployer) the deletion finishes in Delete.
func TestDeletionDryRun(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	res, err := (&appdelete.Service{Store: st}).Delete(ctx, app, "blog", store.DeletionActor{Login: "admin"})
	if err != nil || !res.Done {
		t.Fatalf("%+v %v", res, err)
	}
	if pending, _ := st.PendingAppDeletions(ctx); len(pending) != 0 {
		t.Fatalf("pending: %+v", pending)
	}
}
