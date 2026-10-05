package analytics

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/store"
)

var inf = math.Inf(1)

const (
	svcA = "app-blog-d-aaaaaaa-http@kubernetes"
	svcB = "app-blog-d-bbbbbbb-http@kubernetes"
)

func stats(req, s5 float64, fast float64) deploy.ServiceStats {
	return deploy.ServiceStats{
		Requests: req, Classes: [4]float64{req - s5, 0, 0, s5},
		DurationSum: req * 0.1, DurationCount: req,
		Buckets: map[float64]float64{0.1: fast, inf: req},
	}
}

type fakeSource struct {
	pods []deploy.PodStats
	err  error
}

func (f *fakeSource) TraefikStats(context.Context) ([]deploy.PodStats, error) { return f.pods, f.err }
func (f *fakeSource) Workloads(context.Context) ([]deploy.Workload, error) {
	return []deploy.Workload{
		{Namespace: "app-blog", Name: "d-aaaaaaa", DeploymentID: 1, MetricKey: svcA},
		{Namespace: "app-blog", Name: "d-bbbbbbb", DeploymentID: 2, MetricKey: svcB},
	}, nil
}

type fakeStore struct {
	rows    []store.RequestBucket
	deleted []time.Time
}

func (f *fakeStore) AddRequestMetrics(_ context.Context, b []store.RequestBucket) error {
	f.rows = append(f.rows, b...)
	return nil
}

func (f *fakeStore) DeleteRequestMetricsBefore(_ context.Context, t time.Time) (int64, error) {
	f.deleted = append(f.deleted, t)
	return 0, nil
}

func TestDelta(t *testing.T) {
	d := Delta(stats(10, 1, 8), stats(15, 3, 12))
	if d.Requests != 5 || d.Classes != [4]float64{3, 0, 0, 2} || d.DurationCount != 5 ||
		d.Buckets[0.1] != 4 || d.Buckets[inf] != 5 || math.Abs(d.DurationSum-0.5) > 1e-9 {
		t.Fatalf("delta = %+v", d)
	}
	// Any counter going down is a reset: the current values are the delta.
	if d := Delta(stats(10, 1, 8), stats(4, 0, 4)); d.Requests != 4 || d.Buckets[0.1] != 4 {
		t.Fatalf("reset = %+v", d)
	}
	prev := stats(10, 2, 8)
	cur := stats(12, 1, 10) // 5xx went down although the total grew
	if d := Delta(prev, cur); d.Requests != 12 {
		t.Fatalf("class reset = %+v", d)
	}
	// No histogram: none in the delta.
	if d := Delta(deploy.ServiceStats{Requests: 1}, deploy.ServiceStats{Requests: 3}); d.Requests != 2 || d.Buckets != nil {
		t.Fatalf("plain = %+v", d)
	}
}

func TestCollector(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 5, 12, 0, 2, 0, time.UTC)
	now := t0
	src := &fakeSource{}
	st := &fakeStore{}
	c := &Collector{Source: src, Store: st, now: func() time.Time { return now }}
	podStart := t0.Add(-time.Hour)
	pod := func(name string, started time.Time, svcs map[string]deploy.ServiceStats) deploy.PodStats {
		return deploy.PodStats{Pod: "kube-system/" + name, UID: name + "-uid", Started: started, Services: svcs}
	}

	// First scrape: baseline only (history before the start is unknown).
	src.pods = []deploy.PodStats{pod("traefik-1", podStart, map[string]deploy.ServiceStats{svcA: stats(100, 0, 90)})}
	if got := c.Collect(ctx); len(got) != 0 {
		t.Fatalf("first scrape wrote %+v", got)
	}

	// Next minute: delta for A; B appears on a known pod and counts in full;
	// an unknown service (the control plane's route) is dropped.
	now = t0.Add(time.Minute)
	src.pods = []deploy.PodStats{pod("traefik-1", podStart, map[string]deploy.ServiceStats{
		svcA: stats(110, 2, 98), svcB: stats(3, 0, 3), "paas-paas-http@kubernetes": stats(50, 0, 50),
	})}
	got := c.Collect(ctx)
	want := t0.Truncate(time.Minute) // the minute that just ended
	if len(got) != 2 || got[0].DeploymentID != 1 || got[0].Requests != 10 || got[0].Classes[3] != 2 ||
		got[0].Buckets[0.1] != 8 || !got[0].Minute.Equal(want) || got[1].DeploymentID != 2 || got[1].Requests != 3 {
		t.Fatalf("second scrape = %+v", got)
	}

	// Traefik restarted: new pod (started after the previous scrape) with
	// small counters. Its counts are all new; the old pod is gone.
	now = t0.Add(2 * time.Minute)
	src.pods = []deploy.PodStats{pod("traefik-2", t0.Add(90*time.Second), map[string]deploy.ServiceStats{svcA: stats(7, 1, 7)})}
	got = c.Collect(ctx)
	if len(got) != 1 || got[0].Requests != 7 || got[0].Classes[3] != 1 || !got[0].Minute.Equal(want.Add(time.Minute)) {
		t.Fatalf("after restart = %+v", got)
	}

	// Same pod name and UID, counters reset (in-place restart): reset rule.
	now = t0.Add(3 * time.Minute)
	src.pods = []deploy.PodStats{pod("traefik-2", t0.Add(90*time.Second), map[string]deploy.ServiceStats{svcA: stats(2, 0, 2)})}
	if got = c.Collect(ctx); len(got) != 1 || got[0].Requests != 2 {
		t.Fatalf("counter reset = %+v", got)
	}

	// A failed scrape writes nothing and keeps the baseline; the next one
	// carries both minutes.
	now = t0.Add(4 * time.Minute)
	src.err = errors.New("traefik_service_requests_total not exported")
	if got = c.Collect(ctx); got != nil {
		t.Fatalf("failed scrape = %+v", got)
	}
	src.err = nil
	now = t0.Add(5 * time.Minute)
	src.pods = []deploy.PodStats{pod("traefik-2", t0.Add(90*time.Second), map[string]deploy.ServiceStats{svcA: stats(9, 0, 9)})}
	if got = c.Collect(ctx); len(got) != 1 || got[0].Requests != 7 {
		t.Fatalf("after failure = %+v", got)
	}

	// Idle: nothing written.
	now = t0.Add(6 * time.Minute)
	if got = c.Collect(ctx); len(got) != 0 {
		t.Fatalf("idle = %+v", got)
	}
	if len(st.rows) != 5 {
		t.Fatalf("stored %d rows", len(st.rows))
	}

	// Retention cleanup runs at most hourly.
	c.cleanup(ctx)
	c.cleanup(ctx)
	if len(st.deleted) != 1 || !st.deleted[0].Equal(now.Add(-DefaultRetention)) {
		t.Fatalf("cleanup = %v", st.deleted)
	}
}

func TestBucketizeSumsServicesOfOneDeployment(t *testing.T) {
	m := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	got := Bucketize(map[string]deploy.ServiceStats{
		svcA:    stats(4, 1, 2),
		"alias": stats(6, 0, 6),
		"zero":  {},
	}, map[string]int64{svcA: 1, "alias": 1, "zero": 2}, m)
	if len(got) != 1 || got[0].Requests != 10 || got[0].Classes != [4]int64{9, 0, 0, 1} ||
		got[0].Buckets[0.1] != 8 || got[0].DurationCount != 10 || !got[0].Minute.Equal(m) {
		t.Fatalf("buckets = %+v", got)
	}
}

func TestQuantile(t *testing.T) {
	b := map[float64]float64{0.1: 50, 0.5: 90, 1: 100, inf: 100}
	for _, c := range []struct{ q, want float64 }{
		{0.5, 0.1},   // rank 50 is exactly the 0.1 bucket
		{0.25, 0.05}, // interpolated from 0
		{0.7, 0.3},   // halfway between 0.1 and 0.5
		{0.95, 0.75},
	} {
		got, ok := Quantile(c.q, b)
		if !ok || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("q%.2f = %v %v, want %v", c.q, got, ok, c.want)
		}
	}
	// Rank in +Inf: the highest finite bound.
	if got, _ := Quantile(0.99, map[float64]float64{0.1: 1, inf: 10}); got != 0.1 {
		t.Errorf("+Inf rank = %v", got)
	}
	if _, ok := Quantile(0.5, nil); ok {
		t.Error("empty histogram")
	}
	if _, ok := Quantile(0.5, map[float64]float64{0.1: 0, inf: 0}); ok {
		t.Error("no observations")
	}
}
