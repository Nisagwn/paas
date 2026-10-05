package store

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"time"
)

// Faz 19: request analytics (migration 012). The collector writes one row
// per deployment and minute; the API reads them grouped into steps.

// RequestBucket is the traffic of one deployment in one minute.
type RequestBucket struct {
	DeploymentID int64
	Minute       time.Time
	Requests     int64
	// Classes: 2xx, 3xx, 4xx, 5xx.
	Classes       [4]int64
	DurationSum   float64 // seconds
	DurationCount int64
	// Buckets: cumulative counts by upper bound (seconds; +Inf included).
	Buckets map[float64]float64
}

// MetricPoint is the traffic of one step of a time series.
type MetricPoint struct {
	Time          time.Time
	Requests      int64
	Classes       [4]int64
	DurationSum   float64
	DurationCount int64
	Buckets       map[float64]float64
}

// Add sums o into p (Time is kept).
func (p *MetricPoint) Add(o MetricPoint) {
	p.Requests += o.Requests
	for i := range p.Classes {
		p.Classes[i] += o.Classes[i]
	}
	p.DurationSum += o.DurationSum
	p.DurationCount += o.DurationCount
	for le, v := range o.Buckets {
		if p.Buckets == nil {
			p.Buckets = map[float64]float64{}
		}
		p.Buckets[le] += v
	}
}

// MetricsQuery selects an app's (or one deployment's) traffic in
// [From, To), grouped into steps aligned to multiples of Step since the
// Unix epoch.
type MetricsQuery struct {
	AppID        int64
	DeploymentID int64 // 0: every deployment of the app
	From, To     time.Time
	Step         time.Duration
}

// DeploymentTraffic is one deployment's traffic in a time range.
type DeploymentTraffic struct {
	DeploymentID int64
	CommitSHA    string
	Branch       string
	Status       string
	Production   bool
	Requests     int64
	Classes      [4]int64
}

func encodeBuckets(b map[float64]float64) ([]byte, error) {
	m := make(map[string]float64, len(b))
	for le, v := range b {
		m[formatLe(le)] = v
	}
	return json.Marshal(m)
}

func formatLe(le float64) string {
	if math.IsInf(le, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(le, 'g', -1, 64)
}

// AddRequestMetrics adds the buckets to the stored minutes (a minute written
// twice is summed). Buckets of deleted deployments are skipped.
func (s *Store) AddRequestMetrics(ctx context.Context, buckets []RequestBucket) error {
	if len(buckets) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, b := range buckets {
		hist, err := encodeBuckets(b.Buckets)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO request_metrics (deployment_id, app_id, minute, requests,
				status_2xx, status_3xx, status_4xx, status_5xx, duration_sum, duration_count, duration_buckets)
			SELECT d.id, d.app_id, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb
			FROM deployments d WHERE d.id = $1
			ON CONFLICT (deployment_id, minute) DO UPDATE SET
				requests = request_metrics.requests + EXCLUDED.requests,
				status_2xx = request_metrics.status_2xx + EXCLUDED.status_2xx,
				status_3xx = request_metrics.status_3xx + EXCLUDED.status_3xx,
				status_4xx = request_metrics.status_4xx + EXCLUDED.status_4xx,
				status_5xx = request_metrics.status_5xx + EXCLUDED.status_5xx,
				duration_sum = request_metrics.duration_sum + EXCLUDED.duration_sum,
				duration_count = request_metrics.duration_count + EXCLUDED.duration_count,
				duration_buckets = COALESCE((
					SELECT jsonb_object_agg(k,
						COALESCE((request_metrics.duration_buckets->>k)::float8, 0) +
						COALESCE((EXCLUDED.duration_buckets->>k)::float8, 0))
					FROM (SELECT jsonb_object_keys(request_metrics.duration_buckets)
						UNION SELECT jsonb_object_keys(EXCLUDED.duration_buckets)) keys(k)), '{}')`,
			b.DeploymentID, b.Minute.UTC().Truncate(time.Minute), b.Requests,
			b.Classes[0], b.Classes[1], b.Classes[2], b.Classes[3],
			b.DurationSum, b.DurationCount, string(hist)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteRequestMetricsBefore removes minutes older than t (retention).
func (s *Store) DeleteRequestMetricsBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM request_metrics WHERE minute < $1`, t)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RequestSeries returns the steps of q that saw traffic, oldest first.
func (s *Store) RequestSeries(ctx context.Context, q MetricsQuery) ([]MetricPoint, error) {
	step := q.Step.Seconds()
	if step < 60 {
		step = 60
	}
	const where = `app_id = $1 AND ($2 = 0 OR deployment_id = $2) AND minute >= $3 AND minute < $4`
	const bucket = `to_timestamp(floor(extract(epoch FROM minute) / $5) * $5)`
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+bucket+` AS t, sum(requests), sum(status_2xx), sum(status_3xx), sum(status_4xx),
			sum(status_5xx), sum(duration_sum), sum(duration_count)
		FROM request_metrics WHERE `+where+`
		GROUP BY t ORDER BY t`, q.AppID, q.DeploymentID, q.From, q.To, step)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricPoint{}
	index := map[int64]int{}
	for rows.Next() {
		var p MetricPoint
		if err := rows.Scan(&p.Time, &p.Requests, &p.Classes[0], &p.Classes[1], &p.Classes[2], &p.Classes[3],
			&p.DurationSum, &p.DurationCount); err != nil {
			return nil, err
		}
		p.Time = p.Time.UTC()
		index[p.Time.Unix()] = len(out)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hist, err := s.db.QueryContext(ctx, `
		SELECT `+bucket+` AS t, b.key, sum(b.value::float8)
		FROM request_metrics, jsonb_each_text(duration_buckets) b
		WHERE `+where+`
		GROUP BY t, b.key`, q.AppID, q.DeploymentID, q.From, q.To, step)
	if err != nil {
		return nil, err
	}
	defer hist.Close()
	for hist.Next() {
		var (
			t   time.Time
			key string
			v   float64
		)
		if err := hist.Scan(&t, &key, &v); err != nil {
			return nil, err
		}
		le, err := strconv.ParseFloat(key, 64)
		i, ok := index[t.Unix()]
		if err != nil || !ok {
			continue
		}
		if out[i].Buckets == nil {
			out[i].Buckets = map[float64]float64{}
		}
		out[i].Buckets[le] += v
	}
	return out, hist.Err()
}

// DeploymentTraffic returns, for the app's ready deployments and every
// deployment with traffic in [from, to), the traffic in that range; newest
// deployment first.
func (s *Store) DeploymentTraffic(ctx context.Context, appID int64, from, to time.Time) ([]DeploymentTraffic, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH m AS (
			SELECT deployment_id, sum(requests) r, sum(status_2xx) s2, sum(status_3xx) s3,
				sum(status_4xx) s4, sum(status_5xx) s5
			FROM request_metrics WHERE app_id = $1 AND minute >= $2 AND minute < $3
			GROUP BY deployment_id
		)
		SELECT d.id, d.commit_sha, d.branch, d.status,
			EXISTS (SELECT 1 FROM aliases al WHERE al.deployment_id = d.id AND al.kind = 'production'),
			COALESCE(m.r, 0), COALESCE(m.s2, 0), COALESCE(m.s3, 0), COALESCE(m.s4, 0), COALESCE(m.s5, 0)
		FROM deployments d LEFT JOIN m ON m.deployment_id = d.id
		WHERE d.app_id = $1 AND ((d.status = 'ready' AND d.retired_at IS NULL) OR m.deployment_id IS NOT NULL)
		ORDER BY d.id DESC`, appID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeploymentTraffic{}
	for rows.Next() {
		var t DeploymentTraffic
		if err := rows.Scan(&t.DeploymentID, &t.CommitSHA, &t.Branch, &t.Status, &t.Production, &t.Requests,
			&t.Classes[0], &t.Classes[1], &t.Classes[2], &t.Classes[3]); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
