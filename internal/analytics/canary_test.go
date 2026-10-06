package analytics

import (
	"context"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/deploy"
)

// Faz 21: during a canary the weighted TraefikService's children are
// separate @kubernetescrd series; they add up per deployment with the
// Ingress series of the same deployment.
func TestCollectorCanarySeries(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 5, 12, 0, 2, 0, time.UTC)
	now := t0
	src := &fakeSource{}
	st := &fakeStore{}
	c := &Collector{Source: src, Store: st, now: func() time.Time { return now }}
	crdA, crdB := deploy.CRDMetricKey("app-blog", "d-aaaaaaa"), deploy.CRDMetricKey("app-blog", "d-bbbbbbb")
	if crdA != "app-blog-d-aaaaaaa-http@kubernetescrd" {
		t.Fatalf("CRDMetricKey = %q", crdA)
	}
	pod := func(svcs map[string]deploy.ServiceStats) []deploy.PodStats {
		return []deploy.PodStats{{Pod: "kube-system/traefik-1", UID: "u", Started: t0.Add(-time.Hour), Services: svcs}}
	}
	src.pods = pod(map[string]deploy.ServiceStats{svcA: stats(100, 0, 100)})
	c.Collect(ctx) // baseline

	// The canary starts: the overlay's series appear on the known pod.
	now = t0.Add(time.Minute)
	src.pods = pod(map[string]deploy.ServiceStats{
		svcA: stats(105, 0, 105), crdA: stats(90, 0, 90), crdB: stats(10, 4, 9),
	})
	got := c.Collect(ctx)
	if len(got) != 2 || got[0].DeploymentID != 1 || got[0].Requests != 95 ||
		got[1].DeploymentID != 2 || got[1].Requests != 10 || got[1].Classes[3] != 4 || got[1].Buckets[0.1] != 9 {
		t.Fatalf("canary minute = %+v", got)
	}

	// The canary ends: the CRD series stay frozen (Traefik keeps them), so
	// they add nothing; the Ingress series counts again.
	now = t0.Add(2 * time.Minute)
	src.pods = pod(map[string]deploy.ServiceStats{
		svcA: stats(125, 0, 125), crdA: stats(90, 0, 90), crdB: stats(10, 4, 9),
	})
	got = c.Collect(ctx)
	if len(got) != 1 || got[0].DeploymentID != 1 || got[0].Requests != 20 {
		t.Fatalf("after canary = %+v", got)
	}
}
