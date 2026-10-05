package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/nisagwn/paas/internal/analytics"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 19: request analytics, deployment health and live resource usage.
//
//	GET /api/apps/{name}/analytics?range=1h|24h|7d[&deployment=<id>]
//	GET /api/apps/{name}/health
//	GET /api/apps/{name}/usage

// UsageSource reports live CPU and memory of an app's pods
// (deploy.Kubernetes.AppUsage, metrics.k8s.io).
type UsageSource interface {
	AppUsage(ctx context.Context, app string) ([]deploy.PodUsage, error)
}

// analyticsRanges: range → step of the series.
var analyticsRanges = map[string]struct{ span, step time.Duration }{
	"1h":  {time.Hour, time.Minute},
	"24h": {24 * time.Hour, 10 * time.Minute},
	"7d":  {7 * 24 * time.Hour, time.Hour},
}

// HealthWindow is the window of the deployment health (5xx rate).
const HealthWindow = 15 * time.Minute

// Health thresholds on the 5xx rate of the window.
const (
	healthDegraded = 0.01
	healthFailing  = 0.10
)

// TrafficView counts requests by status class.
type TrafficView struct {
	Requests  int64 `json:"requests"`
	Status2xx int64 `json:"status_2xx"`
	Status3xx int64 `json:"status_3xx"`
	Status4xx int64 `json:"status_4xx"`
	Status5xx int64 `json:"status_5xx"`
	// Errors are 5xx responses; ErrorRate is errors / requests.
	Errors    int64   `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
}

func newTrafficView(requests int64, c [4]int64) TrafficView {
	v := TrafficView{Requests: requests, Status2xx: c[0], Status3xx: c[1], Status4xx: c[2], Status5xx: c[3], Errors: c[3]}
	if requests > 0 {
		v.ErrorRate = float64(c[3]) / float64(requests)
	}
	return v
}

// LatencyView holds latency percentiles.
type LatencyView struct {
	// Milliseconds; nil without a duration histogram.
	P50 *float64 `json:"p50_ms"`
	P95 *float64 `json:"p95_ms"`
	Avg *float64 `json:"avg_ms"`
}

func newLatencyView(p store.MetricPoint) LatencyView {
	var v LatencyView
	ms := func(sec float64) *float64 {
		x := math.Round(sec*1e4) / 10 // 0.1 ms resolution
		return &x
	}
	if q, ok := analytics.Quantile(0.5, p.Buckets); ok {
		v.P50 = ms(q)
	}
	if q, ok := analytics.Quantile(0.95, p.Buckets); ok {
		v.P95 = ms(q)
	}
	if p.DurationCount > 0 {
		v.Avg = ms(p.DurationSum / float64(p.DurationCount))
	}
	return v
}

// PointView is one step of the series (or the totals).
type PointView struct {
	Time time.Time `json:"time"`
	TrafficView
	LatencyView
}

// DeploymentTrafficView is one deployment's traffic in the range.
type DeploymentTrafficView struct {
	DeploymentID int64  `json:"deployment_id"`
	CommitSHA    string `json:"commit_sha"`
	Branch       string `json:"branch"`
	Status       string `json:"status"`
	Production   bool   `json:"production"`
	TrafficView
}

// AnalyticsView is the answer of GET /analytics (Analytics).
type AnalyticsView struct {
	Range        string    `json:"range"`
	StepSeconds  int64     `json:"step_seconds"`
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	DeploymentID int64     `json:"deployment_id,omitempty"`
	// Histogram: latency percentiles are available (Traefik exports the
	// duration histogram).
	Histogram   bool                    `json:"histogram"`
	Totals      PointView               `json:"totals"`
	Series      []PointView             `json:"series"`
	Deployments []DeploymentTrafficView `json:"deployments"`
}

func (s *Server) getAnalytics(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "24h"
	}
	if _, ok := analyticsRanges[rng]; !ok {
		writeError(w, http.StatusBadRequest, ErrRange.Error())
		return
	}
	var depID int64
	if v := r.URL.Query().Get("deployment"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "deployment must be a deployment id")
			return
		}
		d, err := s.Store.GetDeployment(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && d.AppID != app.ID) {
			writeError(w, http.StatusNotFound, "deployment not found")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		depID = id
	}
	out, err := Analytics(r.Context(), s.Store, app.ID, rng, depID, s.clock())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// AnalyticsRanges are the ranges Analytics accepts, shortest first.
var AnalyticsRanges = []string{"1h", "24h", "7d"}

// ErrRange: the range is not one of AnalyticsRanges.
var ErrRange = errors.New("range must be 1h, 24h or 7d")

// Analytics builds the traffic of an app (or of deployment depID when not
// 0) over range rng ending at now. GET /analytics and the web UI share it.
func Analytics(ctx context.Context, st *store.Store, appID int64, rng string, depID int64, now time.Time) (AnalyticsView, error) {
	spec, ok := analyticsRanges[rng]
	if !ok {
		return AnalyticsView{}, ErrRange
	}
	// Steps are aligned to multiples of step; the last one is in progress.
	to := now.UTC().Truncate(spec.step).Add(spec.step)
	from := to.Add(-spec.span)
	points, err := st.RequestSeries(ctx, store.MetricsQuery{
		AppID: appID, DeploymentID: depID, From: from, To: to, Step: spec.step,
	})
	if err != nil {
		return AnalyticsView{}, err
	}
	traffic, err := st.DeploymentTraffic(ctx, appID, from, to)
	if err != nil {
		return AnalyticsView{}, err
	}

	out := AnalyticsView{Range: rng, StepSeconds: int64(spec.step.Seconds()), From: from, To: to, DeploymentID: depID,
		Series: []PointView{}, Deployments: []DeploymentTrafficView{}}
	byTime := make(map[int64]store.MetricPoint, len(points))
	var total store.MetricPoint
	for _, p := range points {
		byTime[p.Time.Unix()] = p
		total.Add(p)
	}
	for t := from; t.Before(to); t = t.Add(spec.step) {
		p := byTime[t.Unix()]
		out.Series = append(out.Series, PointView{Time: t, TrafficView: newTrafficView(p.Requests, p.Classes),
			LatencyView: newLatencyView(p)})
	}
	out.Histogram = len(total.Buckets) > 0
	out.Totals = PointView{Time: from, TrafficView: newTrafficView(total.Requests, total.Classes),
		LatencyView: newLatencyView(total)}
	for _, d := range traffic {
		if d.Requests == 0 || (depID != 0 && d.DeploymentID != depID) {
			continue
		}
		out.Deployments = append(out.Deployments, DeploymentTrafficView{
			DeploymentID: d.DeploymentID, CommitSHA: d.CommitSHA, Branch: d.Branch, Status: d.Status,
			Production: d.Production, TrafficView: newTrafficView(d.Requests, d.Classes),
		})
	}
	return out, nil
}

// DeploymentHealthView is one deployment's health.
type DeploymentHealthView struct {
	DeploymentTrafficView
	// Health: "no_traffic", "healthy" (5xx < 1%), "degraded" (< 10%) or
	// "failing".
	Health string `json:"health"`
}

// HealthView is the answer of GET /health (Health).
type HealthView struct {
	WindowSeconds int64                  `json:"window_seconds"`
	Deployments   []DeploymentHealthView `json:"deployments"`
}

// HealthOf classifies a deployment by its 5xx rate.
func HealthOf(t store.DeploymentTraffic) string {
	switch {
	case t.Requests == 0:
		return "no_traffic"
	case float64(t.Classes[3])/float64(t.Requests) >= healthFailing:
		return "failing"
	case float64(t.Classes[3])/float64(t.Requests) >= healthDegraded:
		return "degraded"
	}
	return "healthy"
}

// getHealth reports the 5xx rate of the last 15 minutes for every ready
// deployment (and any deployment that served requests in the window).
func (s *Server) getHealth(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	out, err := Health(r.Context(), s.Store, app.ID, s.clock())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Health builds the deployment health of the HealthWindow before now.
func Health(ctx context.Context, st *store.Store, appID int64, now time.Time) (HealthView, error) {
	now = now.UTC()
	traffic, err := st.DeploymentTraffic(ctx, appID, now.Add(-HealthWindow), now.Add(time.Minute))
	if err != nil {
		return HealthView{}, err
	}
	out := HealthView{WindowSeconds: int64(HealthWindow.Seconds()), Deployments: []DeploymentHealthView{}}
	for _, d := range traffic {
		out.Deployments = append(out.Deployments, DeploymentHealthView{
			DeploymentTrafficView: DeploymentTrafficView{
				DeploymentID: d.DeploymentID, CommitSHA: d.CommitSHA, Branch: d.Branch, Status: d.Status,
				Production: d.Production, TrafficView: newTrafficView(d.Requests, d.Classes),
			},
			Health: HealthOf(d),
		})
	}
	return out, nil
}

// DeploymentUsageView is the live usage of one deployment's pods.
type DeploymentUsageView struct {
	DeploymentID  int64             `json:"deployment_id"`
	CommitSHA     string            `json:"commit_sha"`
	Branch        string            `json:"branch"`
	CPUMillicores float64           `json:"cpu_millicores"`
	MemoryBytes   int64             `json:"memory_bytes"`
	Pods          []deploy.PodUsage `json:"pods"`
}

// UsageView is the answer of GET /usage (Usage).
type UsageView struct {
	Deployments []DeploymentUsageView `json:"deployments"`
}

// getUsage reports the live CPU and memory of the app's running pods,
// grouped by deployment. Sleeping deployments have no pods and are absent.
func (s *Server) getUsage(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	out, err := Usage(r.Context(), s.Store, s.Usage, app)
	switch {
	case errors.Is(err, ErrNoUsage):
		writeError(w, http.StatusNotImplemented, err.Error())
		return
	case errors.Is(err, deploy.ErrNoMetricsAPI):
		writeError(w, http.StatusServiceUnavailable, "resource metrics unavailable: metrics-server (metrics.k8s.io) is not running")
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ErrNoUsage: there is no usage source (dry-run deployer).
var ErrNoUsage = errors.New("resource usage needs the kubernetes deployer")

// Usage reads the live usage of the app's pods from src, grouped by
// deployment. It fails with ErrNoUsage when src is nil and with
// deploy.ErrNoMetricsAPI without metrics-server.
func Usage(ctx context.Context, st *store.Store, src UsageSource, app store.App) (UsageView, error) {
	if src == nil {
		return UsageView{}, ErrNoUsage
	}
	pods, err := src.AppUsage(ctx, app.Name)
	if err != nil {
		return UsageView{}, err
	}
	out := UsageView{Deployments: []DeploymentUsageView{}}
	index := map[int64]int{}
	for _, p := range pods {
		i, seen := index[p.DeploymentID]
		if !seen {
			d, err := st.GetDeployment(ctx, p.DeploymentID)
			if err != nil || d.AppID != app.ID {
				continue // not (or no longer) a deployment of this app
			}
			i = len(out.Deployments)
			index[p.DeploymentID] = i
			out.Deployments = append(out.Deployments, DeploymentUsageView{
				DeploymentID: d.ID, CommitSHA: d.CommitSHA, Branch: d.Branch, Pods: []deploy.PodUsage{},
			})
		}
		du := &out.Deployments[i]
		du.CPUMillicores += p.CPUMillicores
		du.MemoryBytes += p.MemoryBytes
		du.Pods = append(du.Pods, p)
	}
	return out, nil
}

func (s *Server) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
