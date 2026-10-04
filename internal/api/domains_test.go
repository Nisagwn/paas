package api_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/domains"
	"github.com/nisagwn/paas/internal/routing"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/worker"
)

// noDNS has no records at all.
type noDNS struct{}

func (noDNS) LookupCNAME(context.Context, string) (string, error) {
	return "", errors.New("no such host")
}
func (noDNS) LookupTXT(context.Context, string) ([]string, error) {
	return nil, errors.New("no such host")
}

func setupDomains(t *testing.T, mode string) (*env, *recordingApplier) {
	st := testdb.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ap := &recordingApplier{}
	router := &routing.Syncer{Store: st, Applier: ap, Log: log}
	v := &domains.Verifier{Store: st, Resolver: noDNS{}, Router: router, Domain: domain, Mode: mode, Log: log}
	srv := httptest.NewServer((&api.Server{
		Store: st, Router: router, Domain: domain, APIToken: token, WebhookSecret: secret, Log: log, Domains: v,
	}).Handler())
	t.Cleanup(srv.Close)
	wk := &worker.Worker{Store: st, Pipeline: worker.DryRunPipeline{}, Router: router, Domain: domain, Log: log}
	return &env{t: t, srv: srv, wk: wk}, ap
}

type domainResp struct {
	Hostname   string           `json:"hostname"`
	Status     string           `json:"status"`
	Error      string           `json:"error"`
	Serving    bool             `json:"serving"`
	URL        string           `json:"url"`
	Token      string           `json:"verification_token"`
	DNSRecords []domains.Record `json:"dns_records"`
}

func TestDomainsAPI(t *testing.T) {
	e, ap := setupDomains(t, domains.ModeSkip)
	e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, nil)
	_, d1 := e.push("nisagwn/blog", "main", sha(1))
	e.push("nisagwn/blog", "main", sha(2))
	e.drain()

	for _, bad := range []string{"localhost", "blog.paas.test", "https://www.example.com/", "*.example.com"} {
		if code := e.do("POST", "/api/apps/blog/domains", map[string]string{"hostname": bad}, nil); code != 400 {
			t.Errorf("hostname %q: %d, want 400", bad, code)
		}
	}
	if code := e.do("POST", "/api/apps/nope/domains", map[string]string{"hostname": "www.example.com"}, nil); code != 404 {
		t.Errorf("unknown app: %d", code)
	}

	var d domainResp
	if code := e.do("POST", "/api/apps/blog/domains", map[string]string{"hostname": "WWW.Example.com."}, &d); code != 201 {
		t.Fatalf("add: %d %+v", code, d)
	}
	if d.Hostname != "www.example.com" || d.Status != "active" || !d.Serving || d.URL != "https://www.example.com" ||
		len(d.DNSRecords) != 2 || d.DNSRecords[0].Value != "blog.paas.test" || d.DNSRecords[1].Value != d.Token {
		t.Fatalf("added domain: %+v", d)
	}
	if code := e.do("POST", "/api/apps/blog/domains", map[string]string{"hostname": "www.example.com"}, nil); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}

	// The domain serves production and follows a rollback.
	if got := ap.route("blog", "www.example.com"); got != sha(2) {
		t.Fatalf("custom domain → %q, want %s", got, sha(2))
	}
	if code := e.do("POST", "/api/apps/blog/rollback", map[string]any{"deployment_id": d1["id"]}, nil); code != 200 {
		t.Fatalf("rollback: %d", code)
	}
	if got := ap.route("blog", "www.example.com"); got != sha(1) {
		t.Fatalf("after rollback custom domain → %q, want %s", got, sha(1))
	}

	var list []domainResp
	if code := e.do("GET", "/api/apps/blog/domains", nil, &list); code != 200 || len(list) != 1 || list[0].Hostname != "www.example.com" {
		t.Fatalf("list: %d %+v", code, list)
	}
	if code := e.do("POST", "/api/apps/blog/domains/www.example.com/verify", nil, &d); code != 200 || d.Status != "active" {
		t.Fatalf("verify: %d %+v", code, d)
	}
	if code := e.do("DELETE", "/api/apps/blog/domains/www.example.com", nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if got := ap.route("blog", "www.example.com"); got != "" {
		t.Fatalf("deleted domain still routed → %s", got)
	}
	if code := e.do("DELETE", "/api/apps/blog/domains/www.example.com", nil, nil); code != 404 {
		t.Fatalf("delete again: %d", code)
	}
}

func TestDomainsAPIPendingWithoutDNS(t *testing.T) {
	e, ap := setupDomains(t, domains.ModeDNS)
	e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, nil)
	e.push("nisagwn/blog", "main", sha(1))
	e.drain()

	var d domainResp
	if code := e.do("POST", "/api/apps/blog/domains", map[string]string{"hostname": "www.example.com"}, &d); code != 201 {
		t.Fatalf("add: %d", code)
	}
	if d.Status != "pending" || d.Serving || d.Error == "" {
		t.Fatalf("without DNS records: %+v", d)
	}
	if got := ap.route("blog", "www.example.com"); got != "" {
		t.Fatalf("unverified domain routed → %s", got)
	}
}
