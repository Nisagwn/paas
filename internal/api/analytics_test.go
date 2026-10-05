package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/store"
)

type fakeUsage struct {
	pods []deploy.PodUsage
	err  error
	app  string
}

func (f *fakeUsage) AppUsage(_ context.Context, app string) ([]deploy.PodUsage, error) {
	f.app = app
	return f.pods, f.err
}

type analyticsResp struct {
	Range       string `json:"range"`
	StepSeconds int64  `json:"step_seconds"`
	From, To    time.Time
	Histogram   bool `json:"histogram"`
	Totals      struct {
		Requests  int64    `json:"requests"`
		Errors    int64    `json:"errors"`
		Status4xx int64    `json:"status_4xx"`
		ErrorRate float64  `json:"error_rate"`
		P50       *float64 `json:"p50_ms"`
		P95       *float64 `json:"p95_ms"`
		Avg       *float64 `json:"avg_ms"`
	} `json:"totals"`
	Series []struct {
		Time     time.Time `json:"time"`
		Requests int64     `json:"requests"`
		Errors   int64     `json:"errors"`
		P50      *float64  `json:"p50_ms"`
	} `json:"series"`
	Deployments []struct {
		DeploymentID int64 `json:"deployment_id"`
		Requests     int64 `json:"requests"`
		Production   bool  `json:"production"`
	} `json:"deployments"`
}

func TestAnalyticsAPI(t *testing.T) {
	f := setupTeams(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 5, 12, 30, 20, 0, time.UTC)
	srv := httptest.NewServer((&api.Server{
		Store: f.st, Domain: domain, APIToken: token, WebhookSecret: secret,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Usage: f.usage,
		Now: func() time.Time { return now },
	}).Handler())
	t.Cleanup(srv.Close)
	get := func(path string, out any) int {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}

	inf := math.Inf(1)
	minute := func(m int) time.Time { return now.Truncate(time.Minute).Add(time.Duration(m) * time.Minute) }
	// d2 is production; d1 an older deployment still serving its URL.
	err := f.st.AddRequestMetrics(ctx, []store.RequestBucket{
		{DeploymentID: f.d2.ID, Minute: minute(-1), Requests: 100, Classes: [4]int64{90, 0, 5, 5},
			DurationSum: 5, DurationCount: 100, Buckets: map[float64]float64{0.05: 50, 0.1: 90, 1: 100, inf: 100}},
		{DeploymentID: f.d2.ID, Minute: minute(-5), Requests: 20, Classes: [4]int64{20, 0, 0, 0}},
		{DeploymentID: f.d1.ID, Minute: minute(-10), Requests: 10, Classes: [4]int64{0, 0, 0, 10}},
		{DeploymentID: f.d2.ID, Minute: minute(-120), Requests: 1000, Classes: [4]int64{1000, 0, 0, 0}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var a analyticsResp
	if code := get("/api/apps/blog/analytics?range=1h", &a); code != 200 {
		t.Fatalf("analytics: %d", code)
	}
	if a.Range != "1h" || a.StepSeconds != 60 || len(a.Series) != 60 || !a.To.Equal(minute(1)) {
		t.Fatalf("shape: %s %d %d %v", a.Range, a.StepSeconds, len(a.Series), a.To)
	}
	if a.Totals.Requests != 130 || a.Totals.Errors != 15 || a.Totals.Status4xx != 5 || !a.Histogram {
		t.Fatalf("totals = %+v", a.Totals)
	}
	if a.Totals.P50 == nil || *a.Totals.P50 != 50 || a.Totals.P95 == nil || *a.Totals.P95 != 550 ||
		a.Totals.Avg == nil || *a.Totals.Avg != 50 {
		t.Fatalf("latency = %v %v %v", a.Totals.P50, a.Totals.P95, a.Totals.Avg)
	}
	last := a.Series[58] // the minute that just ended
	if !last.Time.Equal(minute(-1)) || last.Requests != 100 || last.Errors != 5 || last.P50 == nil {
		t.Fatalf("last point = %+v", last)
	}
	if a.Series[59].Requests != 0 || a.Series[59].P50 != nil {
		t.Fatalf("current minute = %+v", a.Series[59])
	}
	if len(a.Deployments) != 2 || a.Deployments[0].DeploymentID != f.d2.ID || !a.Deployments[0].Production ||
		a.Deployments[0].Requests != 120 || a.Deployments[1].Requests != 10 {
		t.Fatalf("deployments = %+v", a.Deployments)
	}

	// 24h includes the older minute; default range is 24h.
	a = analyticsResp{}
	if code := get("/api/apps/blog/analytics", &a); code != 200 || a.Range != "24h" || a.StepSeconds != 600 ||
		len(a.Series) != 144 || a.Totals.Requests != 1130 {
		t.Fatalf("24h: %d %+v", code, a.Totals)
	}
	a = analyticsResp{}
	if code := get("/api/apps/blog/analytics?range=7d", &a); code != 200 || len(a.Series) != 168 {
		t.Fatalf("7d: %d %d", code, len(a.Series))
	}
	// One deployment.
	a = analyticsResp{}
	if code := get(fmt.Sprintf("/api/apps/blog/analytics?range=1h&deployment=%d", f.d1.ID), &a); code != 200 ||
		a.Totals.Requests != 10 || a.Totals.ErrorRate != 1 || a.Histogram || len(a.Deployments) != 1 {
		t.Fatalf("deployment filter: %d %+v", code, a)
	}
	for path, want := range map[string]int{
		"/api/apps/blog/analytics?range=2d":          400,
		"/api/apps/blog/analytics?deployment=x":      400,
		"/api/apps/blog/analytics?deployment=999999": 404,
		"/api/apps/infra/analytics?deployment=1":     404,
		"/api/apps/nope/analytics":                   404,
	} {
		if code := get(path, nil); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}

	// Health: last 15 minutes. d2: 5 of 120 are 5xx (4.2%) → degraded;
	// d1: all 10 → failing.
	var h struct {
		WindowSeconds int64 `json:"window_seconds"`
		Deployments   []struct {
			DeploymentID int64   `json:"deployment_id"`
			Requests     int64   `json:"requests"`
			ErrorRate    float64 `json:"error_rate"`
			Health       string  `json:"health"`
		} `json:"deployments"`
	}
	if code := get("/api/apps/blog/health", &h); code != 200 || h.WindowSeconds != 900 || len(h.Deployments) != 2 {
		t.Fatalf("health: %d %+v", code, h)
	}
	if d := h.Deployments[0]; d.DeploymentID != f.d2.ID || d.Requests != 120 || d.Health != "degraded" {
		t.Fatalf("health d2 = %+v", d)
	}
	if d := h.Deployments[1]; d.DeploymentID != f.d1.ID || d.Health != "failing" || d.ErrorRate != 1 {
		t.Fatalf("health d1 = %+v", d)
	}
	// Twenty minutes later nothing is in the window: ready deployments
	// remain listed without traffic.
	now = now.Add(20 * time.Minute)
	h.Deployments = nil
	get("/api/apps/blog/health", &h)
	if len(h.Deployments) != 2 || h.Deployments[0].Health != "no_traffic" {
		t.Fatalf("idle health = %+v", h.Deployments)
	}
}

func TestUsageAPI(t *testing.T) {
	f := setupTeams(t)
	f.usage.pods = []deploy.PodUsage{
		{Pod: "d-2-a", DeploymentID: f.d2.ID, CPUMillicores: 3, MemoryBytes: 10 << 20},
		{Pod: "d-2-b", DeploymentID: f.d2.ID, CPUMillicores: 1.5, MemoryBytes: 5 << 20},
		{Pod: "d-1-a", DeploymentID: f.d1.ID, CPUMillicores: 0.5, MemoryBytes: 1 << 20},
		{Pod: "stray", DeploymentID: 999999},
	}
	var out struct {
		Deployments []struct {
			DeploymentID  int64   `json:"deployment_id"`
			CommitSHA     string  `json:"commit_sha"`
			CPUMillicores float64 `json:"cpu_millicores"`
			MemoryBytes   int64   `json:"memory_bytes"`
			Pods          []struct {
				Pod string `json:"pod"`
			} `json:"pods"`
		} `json:"deployments"`
	}
	code, body := f.call("carol", "GET", "/api/apps/blog/usage", nil)
	json.Unmarshal([]byte(body), &out)
	if code != 200 || f.usage.app != "blog" || len(out.Deployments) != 2 {
		t.Fatalf("usage: %d %s", code, body)
	}
	if d := out.Deployments[0]; d.DeploymentID != f.d2.ID || d.CommitSHA != f.d2.CommitSHA || d.CPUMillicores != 4.5 ||
		d.MemoryBytes != 15<<20 || len(d.Pods) != 2 {
		t.Fatalf("d2 = %+v", d)
	}

	f.usage.err = fmt.Errorf("wrapped: %w", deploy.ErrNoMetricsAPI)
	if code, _ := f.call("carol", "GET", "/api/apps/blog/usage", nil); code != 503 {
		t.Fatalf("no metrics-server: %d", code)
	}
	f.usage.err = errors.New("boom")
	if code, _ := f.call("carol", "GET", "/api/apps/blog/usage", nil); code != 500 {
		t.Fatalf("error: %d", code)
	}
}
