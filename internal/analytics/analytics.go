// Package analytics records request metrics per deployment (Faz 19).
//
// Every Interval (one minute) the Collector scrapes Traefik's per-service
// counters from every Traefik pod (deploy.Kubernetes.TraefikStats), turns
// them into per-pod deltas and stores the sum per deployment as one row per
// minute (store.AddRequestMetrics). Counters are cumulative and restart at
// zero when a Traefik pod restarts, so:
//
//   - deltas are computed per (pod, service) series, never on sums;
//   - a series whose counters went down was reset: its current value is the
//     delta (everything counted since the restart);
//   - a series seen for the first time counts in full if it appeared after
//     the previous scrape (a new service on a known pod, or a pod started
//     since), and is only a baseline otherwise (the collector's first scrape,
//     or history from before the control plane started).
//
// A failed scrape stores nothing and keeps the previous values, so the next
// successful one carries the whole interval. Rows older than Retention are
// deleted hourly.
package analytics

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/store"
)

// Source is the cluster side (deploy.Kubernetes).
type Source interface {
	TraefikStats(ctx context.Context) ([]deploy.PodStats, error)
	Workloads(ctx context.Context) ([]deploy.Workload, error)
}

// Store is the database side (store.Store).
type Store interface {
	AddRequestMetrics(ctx context.Context, buckets []store.RequestBucket) error
	DeleteRequestMetricsBefore(ctx context.Context, t time.Time) (int64, error)
}

// Defaults.
const (
	DefaultInterval  = time.Minute
	DefaultRetention = 7 * 24 * time.Hour
)

type Collector struct {
	Source Source
	Store  Store
	// Interval between scrapes (default one minute). With the default the
	// scrapes are aligned to the minute so a row holds that minute's traffic.
	Interval time.Duration
	// Retention of the stored minutes (default 7 days).
	Retention time.Duration
	Log       *slog.Logger

	now func() time.Time

	prev     map[seriesKey]deploy.ServiceStats
	prevPods map[string]bool
	prevAt   time.Time
	lastErr  string
	cleaned  time.Time
}

type seriesKey struct{ pod, service string }

// Run scrapes until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	c.defaults()
	interval := c.Interval
	// Align to a few seconds after the minute: a scrape covers the minute
	// that just ended, and that is the minute its delta is stored under.
	if interval == time.Minute {
		next := c.now().Truncate(time.Minute).Add(time.Minute + 2*time.Second)
		if c.wait(ctx, next.Sub(c.now())) {
			return
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c.Collect(ctx)
		c.cleanup(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Collector) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

func (c *Collector) defaults() {
	if c.now == nil {
		c.now = time.Now
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.Retention <= 0 {
		c.Retention = DefaultRetention
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
}

// Collect runs one scrape and stores the deltas. It returns the buckets
// written (for tests and logs).
func (c *Collector) Collect(ctx context.Context) []store.RequestBucket {
	c.defaults()
	now := c.now()
	pods, err := c.Source.TraefikStats(ctx)
	if err != nil {
		c.warn("analytics: traefik metrics unavailable", err)
		return nil
	}
	workloads, err := c.Source.Workloads(ctx)
	if err != nil {
		c.warn("analytics: list workloads", err)
		return nil
	}
	c.warn("", nil)

	deltas := c.advance(pods, now)
	byKey := make(map[string]int64, len(workloads))
	for _, w := range workloads {
		if w.DeploymentID > 0 {
			byKey[w.MetricKey] = w.DeploymentID
		}
	}
	buckets := Bucketize(deltas, byKey, c.minuteOf(now))
	if len(buckets) == 0 {
		return nil
	}
	if err := c.Store.AddRequestMetrics(ctx, buckets); err != nil {
		c.Log.Error("analytics: store", "err", err)
		return nil
	}
	return buckets
}

// minuteOf is the minute a scrape at now is stored under: the minute of the
// previous scrape (whose interval just ended), else the current minute.
func (c *Collector) minuteOf(now time.Time) time.Time {
	at := now
	if c.Interval == time.Minute {
		at = now.Add(-time.Minute)
	}
	return at.UTC().Truncate(time.Minute)
}

// warn logs a scrape problem when it changes, not every minute.
func (c *Collector) warn(msg string, err error) {
	if err == nil {
		if c.lastErr != "" {
			c.Log.Info("analytics: traefik metrics available again")
		}
		c.lastErr = ""
		return
	}
	if err.Error() != c.lastErr {
		c.Log.Warn(msg, "err", err)
	}
	c.lastErr = err.Error()
}

// advance records the scrape and returns the deltas per Traefik service.
func (c *Collector) advance(pods []deploy.PodStats, now time.Time) map[string]deploy.ServiceStats {
	first := c.prev == nil
	cur := map[seriesKey]deploy.ServiceStats{}
	curPods := map[string]bool{}
	out := map[string]deploy.ServiceStats{}
	for _, p := range pods {
		id := p.Pod + "/" + p.UID
		curPods[id] = true
		for svc, s := range p.Services {
			k := seriesKey{id, svc}
			cur[k] = s
			var d deploy.ServiceStats
			if old, ok := c.prev[k]; ok {
				d = Delta(old, s)
			} else if !first && (c.prevPods[id] || p.Started.After(c.prevAt)) {
				d = s // appeared since the previous scrape
			} else {
				continue // baseline
			}
			if d.Requests == 0 && d.DurationCount == 0 {
				continue
			}
			agg := out[svc]
			agg.Add(d)
			out[svc] = agg
		}
	}
	c.prev, c.prevPods, c.prevAt = cur, curPods, now
	return out
}

// Delta is cur - prev for one series, or cur when the counters were reset
// (any counter went down).
func Delta(prev, cur deploy.ServiceStats) deploy.ServiceStats {
	reset := cur.Requests < prev.Requests || cur.DurationCount < prev.DurationCount
	for i := range cur.Classes {
		reset = reset || cur.Classes[i] < prev.Classes[i]
	}
	for le, v := range prev.Buckets {
		reset = reset || cur.Buckets[le] < v
	}
	if reset {
		return cur
	}
	d := deploy.ServiceStats{
		Requests:      cur.Requests - prev.Requests,
		DurationSum:   math.Max(cur.DurationSum-prev.DurationSum, 0),
		DurationCount: cur.DurationCount - prev.DurationCount,
	}
	for i := range cur.Classes {
		d.Classes[i] = cur.Classes[i] - prev.Classes[i]
	}
	if len(cur.Buckets) > 0 {
		d.Buckets = make(map[float64]float64, len(cur.Buckets))
		for le, v := range cur.Buckets {
			d.Buckets[le] = v - prev.Buckets[le]
		}
	}
	return d
}

// Bucketize turns per-service deltas into one row per deployment for the
// given minute. Services that are not deployments (the control plane's own
// route, the dashboard) are dropped.
func Bucketize(deltas map[string]deploy.ServiceStats, deployments map[string]int64, minute time.Time) []store.RequestBucket {
	byID := map[int64]*store.RequestBucket{}
	for svc, d := range deltas {
		id, ok := deployments[svc]
		if !ok {
			continue
		}
		b := byID[id]
		if b == nil {
			b = &store.RequestBucket{DeploymentID: id, Minute: minute}
			byID[id] = b
		}
		b.Requests += round(d.Requests)
		for i := range b.Classes {
			b.Classes[i] += round(d.Classes[i])
		}
		b.DurationSum += d.DurationSum
		b.DurationCount += round(d.DurationCount)
		for le, v := range d.Buckets {
			if b.Buckets == nil {
				b.Buckets = map[float64]float64{}
			}
			b.Buckets[le] += v
		}
	}
	out := make([]store.RequestBucket, 0, len(byID))
	for _, b := range byID {
		if b.Requests > 0 || b.DurationCount > 0 {
			out = append(out, *b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeploymentID < out[j].DeploymentID })
	return out
}

func round(v float64) int64 { return int64(math.Round(v)) }

// cleanup deletes expired minutes at most once an hour.
func (c *Collector) cleanup(ctx context.Context) {
	now := c.now()
	if now.Sub(c.cleaned) < time.Hour {
		return
	}
	c.cleaned = now
	n, err := c.Store.DeleteRequestMetricsBefore(ctx, now.Add(-c.Retention))
	if err != nil {
		c.Log.Error("analytics: retention cleanup", "err", err)
		return
	}
	if n > 0 {
		c.Log.Info("analytics: expired request metrics deleted", "rows", n, "retention", c.Retention)
	}
}

// Quantile estimates the q-quantile (0..1) from cumulative histogram
// buckets like Prometheus's histogram_quantile: linear interpolation
// inside the bucket that holds the rank. ok is false without observations.
// A rank in the +Inf bucket returns the highest finite bound.
func Quantile(q float64, buckets map[float64]float64) (float64, bool) {
	if len(buckets) == 0 {
		return 0, false
	}
	les := make([]float64, 0, len(buckets))
	for le := range buckets {
		les = append(les, le)
	}
	sort.Float64s(les)
	total := buckets[les[len(les)-1]] // the largest bound holds every observation
	if total <= 0 {
		return 0, false
	}
	rank := q * total
	prevLe, prevCount := 0.0, 0.0
	for _, le := range les {
		count := buckets[le]
		if count >= rank {
			if math.IsInf(le, 1) {
				return prevLe, true
			}
			if count == prevCount {
				return le, true
			}
			return prevLe + (le-prevLe)*(rank-prevCount)/(count-prevCount), true
		}
		prevLe, prevCount = le, count
	}
	return prevLe, true
}
