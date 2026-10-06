package deploy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Faz 11: idle detection reads Traefik's Prometheus counters; Faz 19:
// request analytics read the same exposition (status classes and the
// duration histogram). k3s's bundled Traefik exposes them on the "metrics"
// entry point (:9100). Per-service series need
// metrics.prometheus.addServicesLabels, which infra/k8s/05-traefik-config.yaml
// turns on. They are fetched through the API server's pod proxy, which works
// both in the cluster and from a laptop, and needs no extra Service or
// NetworkPolicy.

const (
	traefikRequests   = "traefik_service_requests_total"
	traefikDuration   = "traefik_service_request_duration_seconds"
	traefikEntrypoint = "traefik_entrypoint_requests_total"
)

// ServiceStats are one Traefik service's cumulative counters, summed over
// methods, protocols and codes.
type ServiceStats struct {
	// Requests is the total over all status codes.
	Requests float64
	// Classes counts responses by status class: 2xx, 3xx, 4xx, 5xx.
	Classes [4]float64
	// Duration histogram (seconds); Buckets maps an upper bound (le, +Inf
	// included) to its cumulative count. Empty when Traefik exports no
	// histogram.
	DurationSum, DurationCount float64
	Buckets                    map[float64]float64
}

// HasHistogram reports whether a duration histogram was exported.
func (s ServiceStats) HasHistogram() bool { return len(s.Buckets) > 0 }

// Add sums o into s.
func (s *ServiceStats) Add(o ServiceStats) {
	s.Requests += o.Requests
	for i := range s.Classes {
		s.Classes[i] += o.Classes[i]
	}
	s.DurationSum += o.DurationSum
	s.DurationCount += o.DurationCount
	for le, v := range o.Buckets {
		if s.Buckets == nil {
			s.Buckets = map[float64]float64{}
		}
		s.Buckets[le] += v
	}
}

// PodStats is one Traefik pod's exposition. Counters of a pod start from
// zero when it starts, so consumers compute deltas per pod.
type PodStats struct {
	Pod     string
	UID     string
	Started time.Time
	// Services by Traefik service name (see MetricKey).
	Services map[string]ServiceStats
}

// RequestCounts returns, per Traefik service name (see MetricKey), the total
// number of requests summed over all Traefik pods, codes and methods. Any
// failure is an error: a partial view could make a busy service look idle.
func (k *Kubernetes) RequestCounts(ctx context.Context) (map[string]float64, error) {
	pods, err := k.TraefikStats(ctx)
	if err != nil {
		return nil, err
	}
	total := map[string]float64{}
	for _, p := range pods {
		for svc, s := range p.Services {
			total[IngressMetricKey(svc)] += s.Requests
		}
	}
	return total, nil
}

// IngressMetricKey maps the name a deployment's Service has under
// Traefik's CRD provider (a canary's weighted children, Faz 21:
// CRDMetricKey) to its Ingress provider name (MetricKey); other names are
// returned unchanged. Both series of a deployment then add up to one
// request total. Traefik keeps exporting a CRD series after the canary
// ends, so the sum only grows; a drop (Traefik restart) is a change, which
// idle detection counts as activity anyway.
func IngressMetricKey(svc string) string {
	if base, ok := strings.CutSuffix(svc, "-http@kubernetescrd"); ok {
		return base + "-http@kubernetes"
	}
	return svc
}

// TraefikStats scrapes every running Traefik pod. Any failure is an error,
// so a consumer never mistakes a missing pod for a counter reset.
func (k *Kubernetes) TraefikStats(ctx context.Context) ([]PodStats, error) {
	pods, err := k.client.CoreV1().Pods(k.cfg.TraefikNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: k.cfg.TraefikSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("traefik pods: %w", err)
	}
	var out []PodStats
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		raw, err := k.client.CoreV1().Pods(p.Namespace).
			ProxyGet("http", p.Name, k.cfg.TraefikMetricsPort, "metrics", nil).DoRaw(ctx)
		if err != nil {
			return nil, fmt.Errorf("traefik metrics %s/%s: %w", p.Namespace, p.Name, err)
		}
		svcs, err := ParseServiceStats(raw)
		if err != nil {
			return nil, fmt.Errorf("traefik metrics %s/%s: %w", p.Namespace, p.Name, err)
		}
		ps := PodStats{Pod: p.Namespace + "/" + p.Name, UID: string(p.UID), Services: svcs}
		if p.Status.StartTime != nil {
			ps.Started = p.Status.StartTime.Time
		}
		out = append(out, ps)
	}
	if len(out) == 0 {
		return nil, errors.New("no running traefik pod")
	}
	return out, nil
}

// ParseRequestCounts sums traefik_service_requests_total per service label
// from a Prometheus text exposition.
func ParseRequestCounts(raw []byte) (map[string]float64, error) {
	stats, err := ParseServiceStats(raw)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(stats))
	for svc, s := range stats {
		out[svc] = s.Requests
	}
	return out, nil
}

// ParseServiceStats reads Traefik's per-service request counters and
// duration histogram from a Prometheus text exposition. Requests served
// without per-service series are an error: idleness and traffic are then
// unknowable.
func ParseServiceStats(raw []byte) (map[string]ServiceStats, error) {
	out := map[string]ServiceStats{}
	found, traffic := false, false
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "traefik_") {
			continue // comments, other families
		}
		name, _, _ := strings.Cut(line, "{")
		name, _, _ = strings.Cut(name, " ")
		var kind string
		switch name {
		case traefikEntrypoint:
			traffic = true
			continue
		case traefikRequests:
			kind = "requests"
		case traefikDuration + "_bucket":
			kind = "bucket"
		case traefikDuration + "_sum":
			kind = "sum"
		case traefikDuration + "_count":
			kind = "count"
		default:
			continue
		}
		labels, v, err := parseSample(line[len(name):])
		if err != nil {
			return nil, fmt.Errorf("malformed line %q: %w", line, err)
		}
		if kind == "requests" {
			found = true
		}
		svc := labels["service"]
		if svc == "" {
			continue
		}
		s := out[svc]
		switch kind {
		case "requests":
			s.Requests += v
			if c := codeClass(labels["code"]); c >= 0 {
				s.Classes[c] += v
			}
		case "bucket":
			le, err := strconv.ParseFloat(labels["le"], 64)
			if err != nil {
				return nil, fmt.Errorf("malformed le in %q", line)
			}
			if s.Buckets == nil {
				s.Buckets = map[float64]float64{}
			}
			s.Buckets[le] += v
		case "sum":
			s.DurationSum += v
		case "count":
			s.DurationCount += v
		}
		out[svc] = s
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !found && traffic {
		return nil, errors.New(traefikRequests + " not exported (enable metrics.prometheus.addServicesLabels)")
	}
	return out, nil
}

// codeClass maps "503" to 3 (5xx); -1 for anything outside 2xx-5xx.
func codeClass(code string) int {
	n, err := strconv.Atoi(code)
	if err != nil || n < 200 || n >= 600 {
		return -1
	}
	return n/100 - 2
}

// parseSample parses `{labels} value [timestamp]` (the part after the
// metric name; the label set is optional).
func parseSample(s string) (map[string]string, float64, error) {
	labels := map[string]string{}
	if strings.HasPrefix(s, "{") {
		var err error
		if labels, s, err = parseLabels(s[1:]); err != nil {
			return nil, 0, err
		}
	}
	fields := strings.Fields(s)
	if len(fields) < 1 || len(fields) > 2 {
		return nil, 0, errors.New("want value and optional timestamp")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil, 0, errors.New("malformed value")
	}
	if math.IsNaN(v) {
		v = 0
	}
	return labels, v, nil
}

// parseLabels reads `name="value",...}` and returns the labels and what
// follows the closing brace. Values may contain escaped quotes, backslashes
// and newlines, commas and braces.
func parseLabels(s string) (map[string]string, string, error) {
	labels := map[string]string{}
	for {
		s = strings.TrimLeft(s, " ")
		if rest, ok := strings.CutPrefix(s, "}"); ok {
			return labels, rest, nil
		}
		name, rest, ok := strings.Cut(s, "=")
		if !ok || !strings.HasPrefix(rest, `"`) {
			return nil, "", errors.New("malformed label set")
		}
		var val strings.Builder
		i := 1
		for ; i < len(rest) && rest[i] != '"'; i++ {
			c := rest[i]
			if c == '\\' && i+1 < len(rest) {
				i++
				switch rest[i] {
				case 'n':
					c = '\n'
				default:
					c = rest[i]
				}
			}
			val.WriteByte(c)
		}
		if i >= len(rest) {
			return nil, "", errors.New("unterminated label value")
		}
		labels[strings.TrimSpace(name)] = val.String()
		s = strings.TrimLeft(rest[i+1:], " ")
		s = strings.TrimPrefix(s, ",")
	}
}
