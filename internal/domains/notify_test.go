package domains_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
)

type recorder struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recorder) Emit(_ context.Context, e notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

// expiringCerts is ready and reports a certificate expiry date.
type expiringCerts struct {
	fakeCerts
	notAfter time.Time
}

func (c *expiringCerts) DomainCertificateExpiry(context.Context, string, string) (time.Time, bool, error) {
	return c.notAfter, !c.notAfter.IsZero(), nil
}

// TestDomainNotifications: a domain stuck in pending for a day, a routed
// domain whose DNS breaks (once, not on every check) and a certificate
// that is not renewed are reported (Faz 26).
func TestDomainNotifications(t *testing.T) {
	st, app, v, dns, _, _, c := setup(t)
	ctx := context.Background()
	rec := &recorder{}
	certs := &expiringCerts{fakeCerts: fakeCerts{ready: true}}
	v.Notify, v.Certs = rec, certs

	d, _ := st.AddDomain(ctx, app.ID, "www.example.com", "tok")
	d, _ = v.Check(ctx, d)
	if len(rec.types()) != 0 {
		t.Fatalf("new pending domain reported: %v", rec.types())
	}
	c.t = c.t.Add(25 * time.Hour)
	d, _ = v.Check(ctx, d)
	d, _ = v.Check(ctx, d)
	if ts := rec.types(); len(ts) != 2 || ts[0] != notify.DomainFailed || rec.events[0].Key != rec.events[1].Key {
		t.Fatalf("stuck pending: %+v", rec.events)
	}

	// Verified and active, with a certificate far from expiry: nothing.
	rec.events = nil
	dns.set(func() { dns.cname["www.example.com"] = "blog.paas.test" })
	certs.notAfter = c.t.Add(60 * 24 * time.Hour)
	d, _ = v.Check(ctx, d)
	d, _ = v.Check(ctx, d)
	if d.Status != store.DomainActive || len(rec.types()) != 0 {
		t.Fatalf("active: %s %v", d.Status, rec.types())
	}
	// Ten days left: renewal is stuck.
	certs.notAfter = c.t.Add(10 * 24 * time.Hour)
	d, _ = v.Check(ctx, d)
	if ts := rec.types(); len(ts) != 1 || ts[0] != notify.CertExpiring || rec.events[0].AppID != app.ID ||
		rec.events[0].Link != "/apps/blog/settings#custom-domains" {
		t.Fatalf("expiring: %+v", rec.events)
	}

	// DNS breaks: one event on the transition, none on later checks.
	rec.events = nil
	dns.set(func() { delete(dns.cname, "www.example.com") })
	c.t = c.t.Add(2 * time.Hour)
	d, _ = v.Check(ctx, d)
	c.t = c.t.Add(2 * time.Hour)
	d, _ = v.Check(ctx, d)
	if ts := rec.types(); d.Status != store.DomainError || len(ts) != 1 || ts[0] != notify.DomainFailed ||
		rec.events[0].Severity != notify.SeverityError {
		t.Fatalf("failing: %s %+v", d.Status, rec.events)
	}
}
