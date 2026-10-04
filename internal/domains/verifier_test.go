package domains_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/domains"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

// fakeDNS answers like net.Resolver: LookupCNAME returns the host itself
// when it has no CNAME, and an error when the name does not exist.
type fakeDNS struct {
	mu    sync.Mutex
	cname map[string]string
	txt   map[string][]string
}

func (f *fakeDNS) LookupCNAME(_ context.Context, host string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cname[host]; ok {
		return c, nil
	}
	return "", fmt.Errorf("lookup %s on 127.0.0.53:53: no such host", host)
}

func (f *fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.txt[name]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("lookup %s on 127.0.0.53:53: no such host", name)
}

func (f *fakeDNS) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

type fakeRouter struct {
	mu    sync.Mutex
	syncs int
	err   error
}

func (r *fakeRouter) SyncApp(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncs++
	return r.err
}

type fakeCerts struct{ ready bool }

func (c *fakeCerts) DomainCertificate(context.Context, string, string) (bool, string, error) {
	if c.ready {
		return true, "", nil
	}
	return false, "issuing", nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup(t *testing.T) (*store.Store, store.App, *domains.Verifier, *fakeDNS, *fakeRouter, *fakeCerts, *clock) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("a", 40), "main", "")
	st.MarkReady(ctx, d, []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}})

	dns := &fakeDNS{cname: map[string]string{}, txt: map[string][]string{}}
	r, certs, c := &fakeRouter{}, &fakeCerts{}, &clock{t: time.Now()}
	v := &domains.Verifier{
		Store: st, Resolver: dns, Router: r, Certs: certs, Domain: "paas.test", Mode: domains.ModeDNS,
		Interval: time.Minute, Recheck: time.Hour, Grace: 72 * time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: c.now,
	}
	return st, app, v, dns, r, certs, c
}

func get(t *testing.T, st *store.Store, appID int64, host string) store.Domain {
	t.Helper()
	d, err := st.GetDomain(context.Background(), appID, host)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestVerifyByCNAMEThenCertificate(t *testing.T) {
	st, app, v, dns, r, certs, c := setup(t)
	ctx := context.Background()
	d, _ := st.AddDomain(ctx, app.ID, "www.example.com", "tok")

	// No records yet: stays pending, the error says what is missing.
	d, err := v.Check(ctx, d)
	if err != nil || d.Status != store.DomainPending || d.Routed || !strings.Contains(d.Error, "no such host") {
		t.Fatalf("no records: %+v %v", d, err)
	}

	// A CNAME elsewhere is reported.
	dns.set(func() { dns.cname["www.example.com"] = "other.host.net." })
	d, _ = v.Check(ctx, d)
	if d.Status != store.DomainPending || !strings.Contains(d.Error, "CNAME points to other.host.net, not blog.paas.test") {
		t.Fatalf("wrong cname: %+v", d)
	}

	// The right CNAME (trailing dot, any case) verifies and routes the
	// domain; it waits for its certificate before it is active.
	dns.set(func() { dns.cname["www.example.com"] = "Blog.Paas.Test." })
	d, err = v.Check(ctx, d)
	if err != nil || d.Status != store.DomainVerified || !d.Routed || d.VerifiedBy != "cname" ||
		d.VerifiedAt == nil || !strings.Contains(d.Error, "waiting for the certificate") {
		t.Fatalf("verified: %+v %v", d, err)
	}
	if r.syncs != 1 {
		t.Fatalf("route syncs = %d, want 1", r.syncs)
	}

	certs.ready = true
	c.t = c.t.Add(time.Minute)
	if n := v.CheckDue(ctx); n != 0 {
		t.Fatalf("CheckDue failed %d", n)
	}
	if d = get(t, st, app.ID, "www.example.com"); d.Status != store.DomainActive || d.Error != "" {
		t.Fatalf("active: %+v", d)
	}

	// Active domains are re-validated only every Recheck.
	syncs := r.syncs
	dns.set(func() { delete(dns.cname, "www.example.com") })
	c.t = c.t.Add(30 * time.Minute)
	v.CheckDue(ctx)
	if d = get(t, st, app.ID, "www.example.com"); d.Status != store.DomainActive {
		t.Fatalf("checked before Recheck: %+v", d)
	}

	// A failed re-validation keeps serving during the grace period ...
	c.t = c.t.Add(time.Hour)
	v.CheckDue(ctx)
	d = get(t, st, app.ID, "www.example.com")
	if d.Status != store.DomainError || !d.Routed || d.FailingSince == nil || !strings.Contains(d.Error, "still served until") {
		t.Fatalf("failing: %+v", d)
	}
	if r.syncs != syncs {
		t.Fatal("routing must not change during the grace period")
	}
	// ... and the route is removed after it.
	c.t = c.t.Add(73 * time.Hour)
	v.CheckDue(ctx)
	d = get(t, st, app.ID, "www.example.com")
	if d.Status != store.DomainError || d.Routed || !strings.Contains(d.Error, "grace period ended") || r.syncs != syncs+1 {
		t.Fatalf("after grace: %+v syncs=%d", d, r.syncs)
	}

	// Fixing DNS brings it back.
	dns.set(func() { dns.cname["www.example.com"] = "blog.paas.test" })
	c.t = c.t.Add(time.Minute)
	v.CheckDue(ctx)
	if d = get(t, st, app.ID, "www.example.com"); d.Status != store.DomainActive || !d.Routed || d.FailingSince != nil {
		t.Fatalf("recovered: %+v", d)
	}
}

func TestVerifyByTXTAndApexCNAME(t *testing.T) {
	st, app, v, dns, r, certs, _ := setup(t)
	ctx := context.Background()
	certs.ready = true

	apex, _ := st.AddDomain(ctx, app.ID, "example.org", "tok-1")
	dns.set(func() {
		dns.cname["example.org"] = "example.org." // A record, no CNAME
		dns.txt["_paas-challenge.example.org"] = []string{"unrelated", "tok-1"}
	})
	if d, err := v.Check(ctx, apex); err != nil || d.Status != store.DomainActive || d.VerifiedBy != "txt" {
		t.Fatalf("txt: %+v %v", d, err)
	}

	// A CNAME to the platform apex also verifies.
	other, _ := st.AddDomain(ctx, app.ID, "shop.example.net", "tok-2")
	dns.set(func() { dns.cname["shop.example.net"] = "paas.test." })
	if d, _ := v.Check(ctx, other); d.Status != store.DomainActive || d.VerifiedBy != "cname" {
		t.Fatalf("apex cname: %+v", d)
	}

	// A wrong TXT value does not.
	bad, _ := st.AddDomain(ctx, app.ID, "bad.example.net", "tok-3")
	dns.set(func() {
		dns.cname["bad.example.net"] = "bad.example.net"
		dns.txt["_paas-challenge.bad.example.net"] = []string{"tok-1"}
	})
	if d, _ := v.Check(ctx, bad); d.Status != store.DomainPending ||
		!strings.Contains(d.Error, "no CNAME record") || !strings.Contains(d.Error, "does not contain the verification token") {
		t.Fatalf("wrong txt: %+v", d)
	}

	// A routing failure keeps the domain verified and is retried.
	r.err = errors.New("api down")
	failing, _ := st.AddDomain(ctx, app.ID, "x.example.net", "tok-4")
	dns.set(func() { dns.cname["x.example.net"] = "blog.paas.test" })
	d, err := v.Check(ctx, failing)
	if err == nil || d.Status != store.DomainVerified || !d.Routed || !strings.Contains(d.Error, "routing failed") {
		t.Fatalf("routing failure: %+v %v", d, err)
	}
	r.err = nil
	if d, _ := v.Check(ctx, d); d.Status != store.DomainActive {
		t.Fatalf("after retry: %+v", d)
	}
}

func TestSkipModeAndNoProduction(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	v := &domains.Verifier{Store: st, Domain: "paas.test", Mode: domains.ModeSkip,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d, _ := st.AddDomain(ctx, app.ID, "www.example.com", "tok")
	d, err := v.Check(ctx, d)
	if err != nil || d.Status != store.DomainVerified || !d.Routed || d.VerifiedBy != "skip" ||
		d.Error != "waiting for the first production deployment" {
		t.Fatalf("skip without production: %+v %v", d, err)
	}
	dep, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("b", 40), "main", "")
	st.MarkReady(ctx, dep, []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}})
	if d, _ = v.Check(ctx, d); d.Status != store.DomainActive {
		t.Fatalf("skip with production: %+v", d)
	}
	// A deleted domain is not resurrected.
	st.DeleteDomain(ctx, app.ID, d.Hostname)
	if _, err := v.Check(ctx, d); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}
