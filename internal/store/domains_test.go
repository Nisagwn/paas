package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func routesByHost(t *testing.T, st *store.Store, appID int64) map[string]store.AliasRoute {
	t.Helper()
	rs, err := st.AliasRoutes(context.Background(), appID)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]store.AliasRoute{}
	for _, r := range rs {
		m[r.Hostname] = r
	}
	return m
}

func TestDomainsFollowProduction(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	other, _ := st.CreateApp(ctx, "shop", "nisagwn/shop", "main")

	d, err := st.AddDomain(ctx, app.ID, "www.example.com", "tok")
	if err != nil || d.Status != store.DomainPending || d.Routed || d.AppName != "blog" || d.Token != "tok" {
		t.Fatalf("add: %+v %v", d, err)
	}
	// Hostnames are unique across apps.
	if _, err := st.AddDomain(ctx, other.ID, "www.example.com", "x"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate hostname: %v", err)
	}

	prod := []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}}
	d1, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "")
	d2, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(2), "main", "")
	st.MarkReady(ctx, d1, prod)
	st.MarkReady(ctx, d2, prod)

	// Pending domains are not routed.
	if _, ok := routesByHost(t, st, app.ID)["www.example.com"]; ok {
		t.Fatal("pending domain must not be routed")
	}

	d.Status, d.Routed, d.VerifiedBy = store.DomainVerified, true, "cname"
	saved, err := st.SaveDomainCheck(ctx, d)
	if err != nil || !saved.Routed || saved.LastCheckedAt == nil || saved.VerifiedBy != "cname" {
		t.Fatalf("save check: %+v %v", saved, err)
	}
	r := routesByHost(t, st, app.ID)["www.example.com"]
	if r.Kind != store.AliasCustom || r.DeploymentID != d2.ID || r.CommitSHA != sha(2) {
		t.Fatalf("custom route = %+v", r)
	}

	// Rollback moves the custom domain with production.
	if _, err := st.Rollback(ctx, app.ID, d1.ID); err != nil {
		t.Fatal(err)
	}
	if r := routesByHost(t, st, app.ID)["www.example.com"]; r.CommitSHA != sha(1) {
		t.Fatalf("after rollback custom domain → %s, want %s", r.CommitSHA, sha(1))
	}
	if n := len(routesByHost(t, st, other.ID)); n != 0 {
		t.Fatalf("other app has %d routes", n)
	}

	if err := st.DeleteDomain(ctx, app.ID, "www.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteDomain(ctx, app.ID, "www.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := st.SaveDomainCheck(ctx, saved); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("saving a deleted domain: %v", err)
	}
	if _, ok := routesByHost(t, st, app.ID)["www.example.com"]; ok {
		t.Fatal("deleted domain still routed")
	}
}

func TestDomainLimitAndCheck(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	for i := 0; i < store.MaxDomainsPerApp; i++ {
		if _, err := st.AddDomain(ctx, app.ID, fmt.Sprintf("d%d.example.com", i), "t"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddDomain(ctx, app.ID, "one-more.example.com", "t"); !errors.Is(err, store.ErrTooManyDomains) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := st.AddDomain(ctx, app.ID, "Upper.Example.com", "t"); err == nil {
		t.Fatal("the schema must reject uppercase hostnames")
	}
	list, err := st.ListDomains(ctx, 0)
	if err != nil || len(list) != store.MaxDomainsPerApp {
		t.Fatalf("list all: %d %v", len(list), err)
	}
	if ok, _ := st.HasProduction(ctx, app.ID); ok {
		t.Fatal("no production yet")
	}
	if _, err := st.GetDomain(ctx, app.ID, "nope.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
}
