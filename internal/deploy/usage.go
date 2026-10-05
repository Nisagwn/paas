package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nisagwn/paas/internal/naming"
)

// Faz 19: live CPU and memory of deployment pods from the resource metrics
// API (metrics.k8s.io, served by metrics-server, which ships with k3s). It is
// read as raw JSON through the clientset's REST client, so no metrics client
// library is needed.

// ErrNoMetricsAPI means metrics.k8s.io is not served (no metrics-server).
var ErrNoMetricsAPI = errors.New("deploy: resource metrics API (metrics.k8s.io) unavailable")

// PodUsage is the current usage of one pod of a deployment.
type PodUsage struct {
	Pod          string `json:"pod"`
	DeploymentID int64  `json:"deployment_id"`
	CommitSHA    string `json:"-"`
	// CPU in millicores, memory (working set) in bytes.
	CPUMillicores float64 `json:"cpu_millicores"`
	MemoryBytes   int64   `json:"memory_bytes"`
	// Limits of the pod's containers (0: none).
	CPULimitMillicores float64 `json:"cpu_limit_millicores"`
	MemoryLimitBytes   int64   `json:"memory_limit_bytes"`
	// When metrics-server sampled it, over which window.
	Timestamp     time.Time `json:"timestamp"`
	WindowSeconds float64   `json:"window_seconds"`
}

type podMetricsList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Timestamp  time.Time `json:"timestamp"`
		Window     string    `json:"window"`
		Containers []struct {
			Name  string            `json:"name"`
			Usage map[string]string `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// AppUsage returns the usage of every running pod paas manages in the app's
// namespace, sorted by deployment (newest first) and pod name.
func (k *Kubernetes) AppUsage(ctx context.Context, app string) ([]PodUsage, error) {
	ns := naming.Namespace(app)
	selector := LabelManagedBy + "=" + ManagedBy
	pods, err := k.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return []PodUsage{}, nil
	}
	rc := k.client.Discovery().RESTClient()
	if rc == nil {
		return nil, ErrNoMetricsAPI
	}
	raw, err := rc.Get().AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", ns, "pods").
		Param("labelSelector", selector).DoRaw(ctx)
	if apierrors.IsNotFound(err) || apierrors.IsServiceUnavailable(err) {
		return nil, fmt.Errorf("%w: %v", ErrNoMetricsAPI, err)
	}
	if err != nil {
		return nil, fmt.Errorf("pod metrics: %w", err)
	}
	var list podMetricsList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("pod metrics: %w", err)
	}
	type sample struct {
		cpu  float64
		mem  int64
		ts   time.Time
		wind float64
	}
	byPod := map[string]sample{}
	for _, it := range list.Items {
		var s sample
		s.ts = it.Timestamp
		if d, err := time.ParseDuration(it.Window); err == nil {
			s.wind = d.Seconds()
		}
		for _, c := range it.Containers {
			if q, err := resource.ParseQuantity(c.Usage["cpu"]); err == nil {
				s.cpu += float64(q.ScaledValue(resource.Nano)) / 1e6
			}
			if q, err := resource.ParseQuantity(c.Usage["memory"]); err == nil {
				s.mem += q.Value()
			}
		}
		byPod[it.Metadata.Name] = s
	}

	out := []PodUsage{}
	for _, p := range pods.Items {
		s, ok := byPod[p.Name]
		if !ok || p.DeletionTimestamp != nil {
			continue // not running long enough to be sampled, or terminating
		}
		id, _ := strconv.ParseInt(p.Labels[LabelDeploymentID], 10, 64)
		u := PodUsage{
			Pod: p.Name, DeploymentID: id, CommitSHA: p.Labels[LabelCommit],
			CPUMillicores: s.cpu, MemoryBytes: s.mem, Timestamp: s.ts, WindowSeconds: s.wind,
		}
		for _, c := range p.Spec.Containers {
			if q, ok := c.Resources.Limits["cpu"]; ok {
				u.CPULimitMillicores += float64(q.MilliValue())
			}
			if q, ok := c.Resources.Limits["memory"]; ok {
				u.MemoryLimitBytes += q.Value()
			}
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeploymentID != out[j].DeploymentID {
			return out[i].DeploymentID > out[j].DeploymentID
		}
		return out[i].Pod < out[j].Pod
	})
	return out, nil
}
