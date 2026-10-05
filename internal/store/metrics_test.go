package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func TestRequestMetrics(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	other, _ := st.CreateApp(ctx, "shop", "nisagwn/shop", "main")
	prod := deploy(t, st, app, 1, "main")
	prev := deploy(t, st, app, 2, "feature")
	foreign := deploy(t, st, other, 3, "main")
	// A deployment without traffic and not ready does not show up.
	if _, _, err := st.EnqueueDeployment(ctx, app.ID, sha(4), "wip", ""); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	inf := math.Inf(1)
	bucket := func(d store.Deployment, minute int, req, s5 int64, hist map[float64]float64) store.RequestBucket {
		return store.RequestBucket{
			DeploymentID: d.ID, Minute: base.Add(time.Duration(minute) * time.Minute).Add(17 * time.Second),
			Requests: req, Classes: [4]int64{req - s5, 0, 0, s5},
			DurationSum: float64(req) * 0.05, DurationCount: req, Buckets: hist,
		}
	}
	err := st.AddRequestMetrics(ctx, []store.RequestBucket{
		bucket(prod, 0, 10, 1, map[float64]float64{0.1: 8, inf: 10}),
		bucket(prod, 1, 5, 0, map[float64]float64{0.1: 5, inf: 5}),
		bucket(prev, 1, 4, 2, nil),
		bucket(prod, 12, 3, 0, nil),
		bucket(foreign, 0, 100, 0, nil),
		{DeploymentID: 999999, Minute: base, Requests: 1}, // deleted deployment: skipped
	})
	if err != nil {
		t.Fatal(err)
	}
	// The same minute again is summed, histograms key by key.
	if err := st.AddRequestMetrics(ctx, []store.RequestBucket{
		bucket(prod, 0, 2, 1, map[float64]float64{0.1: 1, 0.5: 2, inf: 2}),
	}); err != nil {
		t.Fatal(err)
	}

	pts, err := st.RequestSeries(ctx, store.MetricsQuery{AppID: app.ID, From: base, To: base.Add(time.Hour), Step: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || !pts[0].Time.Equal(base) || !pts[1].Time.Equal(base.Add(10*time.Minute)) {
		t.Fatalf("points = %+v", pts)
	}
	p := pts[0]
	if p.Requests != 21 || p.Classes[3] != 4 || p.Classes[0] != 17 || p.DurationCount != 21 {
		t.Fatalf("first step = %+v", p)
	}
	if p.Buckets[0.1] != 14 || p.Buckets[0.5] != 2 || p.Buckets[inf] != 17 {
		t.Fatalf("histogram = %v", p.Buckets)
	}
	if pts[1].Requests != 3 || pts[1].Buckets != nil {
		t.Fatalf("second step = %+v", pts[1])
	}

	// One deployment, one-minute steps, a range that ends before minute 12.
	pts, err = st.RequestSeries(ctx, store.MetricsQuery{AppID: app.ID, DeploymentID: prev.ID, From: base,
		To: base.Add(10 * time.Minute), Step: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].Requests != 4 || !pts[0].Time.Equal(base.Add(time.Minute)) {
		t.Fatalf("deployment series = %+v", pts)
	}

	traffic, err := st.DeploymentTraffic(ctx, app.ID, base, base.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(traffic) != 2 || traffic[0].DeploymentID != prev.ID || traffic[1].DeploymentID != prod.ID {
		t.Fatalf("traffic = %+v", traffic)
	}
	if !traffic[1].Production || traffic[1].Requests != 17 || traffic[1].Classes[3] != 2 ||
		traffic[0].Production || traffic[0].Requests != 4 {
		t.Fatalf("traffic = %+v", traffic)
	}
	// Outside any traffic: ready deployments with zeros.
	traffic, _ = st.DeploymentTraffic(ctx, app.ID, base.Add(time.Hour), base.Add(2*time.Hour))
	if len(traffic) != 2 || traffic[0].Requests != 0 || traffic[1].Requests != 0 {
		t.Fatalf("idle traffic = %+v", traffic)
	}

	// Retention.
	n, err := st.DeleteRequestMetricsBefore(ctx, base.Add(5*time.Minute))
	if err != nil || n != 4 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	pts, _ = st.RequestSeries(ctx, store.MetricsQuery{AppID: app.ID, From: base, To: base.Add(time.Hour), Step: time.Hour})
	if len(pts) != 1 || pts[0].Requests != 3 {
		t.Fatalf("after retention = %+v", pts)
	}
}
