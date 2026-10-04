package deploy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Faz 11: idle detection reads Traefik's Prometheus counters. k3s's bundled
// Traefik exposes them on the "metrics" entry point (:9100) by default. They
// are fetched through the API server's pod proxy, which works both in the
// cluster and from a laptop, and needs no extra Service or NetworkPolicy.

const traefikRequests = "traefik_service_requests_total"

// RequestCounts returns, per Traefik service name (see MetricKey), the total
// number of requests summed over all Traefik pods, codes and methods. Any
// failure is an error: a partial view could make a busy service look idle.
func (k *Kubernetes) RequestCounts(ctx context.Context) (map[string]float64, error) {
	pods, err := k.client.CoreV1().Pods(k.cfg.TraefikNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: k.cfg.TraefikSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("traefik pods: %w", err)
	}
	total := map[string]float64{}
	n := 0
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		raw, err := k.client.CoreV1().Pods(p.Namespace).
			ProxyGet("http", p.Name, k.cfg.TraefikMetricsPort, "metrics", nil).DoRaw(ctx)
		if err != nil {
			return nil, fmt.Errorf("traefik metrics %s/%s: %w", p.Namespace, p.Name, err)
		}
		counts, err := ParseRequestCounts(raw)
		if err != nil {
			return nil, fmt.Errorf("traefik metrics %s/%s: %w", p.Namespace, p.Name, err)
		}
		for svc, v := range counts {
			total[svc] += v
		}
		n++
	}
	if n == 0 {
		return nil, errors.New("no running traefik pod")
	}
	return total, nil
}

// ParseRequestCounts sums traefik_service_requests_total per service label
// from a Prometheus text exposition.
func ParseRequestCounts(raw []byte) (map[string]float64, error) {
	out := map[string]float64{}
	found, traffic := false, false
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		rest, ok := strings.CutPrefix(line, traefikRequests+"{")
		if !ok {
			if strings.HasPrefix(line, "traefik_entrypoint_requests_total{") {
				traffic = true
			}
			continue
		}
		found = true
		labels, value, ok := strings.Cut(rest, "} ")
		if !ok {
			return nil, fmt.Errorf("malformed line %q", line)
		}
		if i := strings.IndexByte(value, ' '); i >= 0 {
			value = value[:i] // optional timestamp
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("malformed value in %q", line)
		}
		if svc := labelValue(labels, "service"); svc != "" {
			out[svc] += v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !found && traffic {
		// Requests were served but not per service: service labels are
		// disabled, and idleness is unknowable.
		return nil, errors.New(traefikRequests + " not exported (enable metrics.prometheus.addServicesLabels)")
	}
	return out, nil
}

// labelValue extracts name="value" from a Prometheus label set. Values in
// Traefik's service labels contain no escaped quotes.
func labelValue(labels, name string) string {
	for _, part := range strings.Split(labels, ",") {
		k, v, ok := strings.Cut(part, "=")
		if ok && strings.TrimSpace(k) == name {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}
