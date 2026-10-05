package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const exposition = `# HELP traefik_service_requests_total How many HTTP requests processed on a service.
# TYPE traefik_service_requests_total counter
traefik_service_requests_total{code="200",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 90
traefik_service_requests_total{code="201",method="POST",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 5
traefik_service_requests_total{code="304",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 3
traefik_service_requests_total{code="404",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 2
traefik_service_requests_total{code="502",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 4
traefik_service_requests_total{code="101",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 1
traefik_service_requests_total{code="200",method="GET",protocol="http",service="odd,name \"x\" {y}@kubernetes"} 7 1700000000000
# TYPE traefik_service_request_duration_seconds histogram
traefik_service_request_duration_seconds_bucket{code="200",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes",le="0.1"} 80
traefik_service_request_duration_seconds_bucket{code="200",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes",le="0.5"} 88
traefik_service_request_duration_seconds_bucket{code="200",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes",le="+Inf"} 90
traefik_service_request_duration_seconds_bucket{code="502",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes",le="0.1"} 0
traefik_service_request_duration_seconds_bucket{code="502",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes",le="0.5"} 1
traefik_service_request_duration_seconds_bucket{code="502",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes",le="+Inf"} 4
traefik_service_request_duration_seconds_sum{code="200",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 4.5
traefik_service_request_duration_seconds_sum{code="502",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 40
traefik_service_request_duration_seconds_count{code="200",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 90
traefik_service_request_duration_seconds_count{code="502",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 4
traefik_entrypoint_requests_total{code="200",entrypoint="web",method="GET",protocol="http"} 112
traefik_config_reloads_total 3
go_goroutines 42
`

func TestParseServiceStats(t *testing.T) {
	got, err := ParseServiceStats([]byte(exposition))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("services = %v", got)
	}
	s := got["app-blog-d-a3f9c1d-http@kubernetes"]
	// 101 counts as a request but in no class.
	if s.Requests != 105 || s.Classes != [4]float64{95, 3, 2, 4} {
		t.Fatalf("requests %v classes %v", s.Requests, s.Classes)
	}
	if !s.HasHistogram() || s.Buckets[0.1] != 80 || s.Buckets[0.5] != 89 || s.Buckets[math.Inf(1)] != 94 {
		t.Fatalf("buckets = %v", s.Buckets)
	}
	if s.DurationSum != 44.5 || s.DurationCount != 94 {
		t.Fatalf("sum %v count %v", s.DurationSum, s.DurationCount)
	}
	// Escaped quotes, commas and braces in a label value; a timestamp.
	if o := got[`odd,name "x" {y}@kubernetes`]; o.Requests != 7 || o.HasHistogram() {
		t.Fatalf("odd service = %+v", o)
	}

	for name, raw := range map[string]string{
		"bad value":         `traefik_service_requests_total{service="a"} x`,
		"unterminated":      `traefik_service_requests_total{service="a} 1`,
		"bad le":            `traefik_service_request_duration_seconds_bucket{service="a",le="x"} 1`,
		"no labels service": `traefik_entrypoint_requests_total{code="200"} 5`,
	} {
		if _, err := ParseServiceStats([]byte(raw + "\n")); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	// A histogram without request counters still parses (no traffic yet).
	if got, err := ParseServiceStats([]byte("traefik_config_reloads_total 1\n")); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
}

// fakeAPIServer serves pods and pod metrics of app-blog; metrics answers
// 404 when noMetrics is set (no metrics-server).
func fakeAPIServer(t *testing.T, noMetrics bool) *Kubernetes {
	t.Helper()
	limits := corev1.ResourceList{"cpu": resource.MustParse("500m"), "memory": resource.MustParse("256Mi")}
	pod := func(name, id string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "app-blog",
				Labels: map[string]string{LabelManagedBy: ManagedBy, LabelDeploymentID: id, LabelCommit: "a3f9c1d"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Limits: limits}}}},
		}
	}
	pods := corev1.PodList{
		TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
		Items:    []corev1.Pod{pod("d-a3f9c1d-1", "7"), pod("d-a3f9c1d-2", "7"), pod("d-0000001-1", "9"), pod("d-new-1", "10")},
	}
	metrics := `{"kind":"PodMetricsList","apiVersion":"metrics.k8s.io/v1beta1","items":[
		{"metadata":{"name":"d-a3f9c1d-1","namespace":"app-blog"},"timestamp":"2026-10-05T12:00:00Z","window":"15s",
		 "containers":[{"name":"app","usage":{"cpu":"2500000n","memory":"10Mi"}}]},
		{"metadata":{"name":"d-a3f9c1d-2","namespace":"app-blog"},"timestamp":"2026-10-05T12:00:00Z","window":"15s",
		 "containers":[{"name":"app","usage":{"cpu":"1m","memory":"20480Ki"}}]},
		{"metadata":{"name":"d-0000001-1","namespace":"app-blog"},"timestamp":"2026-10-05T12:00:01Z","window":"20s",
		 "containers":[{"name":"app","usage":{"cpu":"0","memory":"5Mi"}},{"name":"side","usage":{"cpu":"125m","memory":"1Mi"}}]}
	]}`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/namespaces/app-blog/pods", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("labelSelector") != LabelManagedBy+"="+ManagedBy {
			t.Errorf("pods selector = %q", r.URL.Query().Get("labelSelector"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pods)
	})
	mux.HandleFunc("GET /apis/metrics.k8s.io/v1beta1/namespaces/app-blog/pods", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if noMetrics {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
			return
		}
		w.Write([]byte(metrics))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Domain = "paas.test"
	k, err := New(cs, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestAppUsage(t *testing.T) {
	k := fakeAPIServer(t, false)
	got, err := k.AppUsage(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	// d-new-1 has no sample yet; deployments newest first.
	if len(got) != 3 || got[0].Pod != "d-0000001-1" || got[1].Pod != "d-a3f9c1d-1" || got[2].Pod != "d-a3f9c1d-2" {
		t.Fatalf("usage = %+v", got)
	}
	if u := got[0]; u.DeploymentID != 9 || u.CPUMillicores != 125 || u.MemoryBytes != 6<<20 || u.WindowSeconds != 20 {
		t.Fatalf("multi-container pod = %+v", u)
	}
	if u := got[1]; u.DeploymentID != 7 || u.CPUMillicores != 2.5 || u.MemoryBytes != 10<<20 ||
		u.CPULimitMillicores != 500 || u.MemoryLimitBytes != 256<<20 || u.Timestamp.IsZero() {
		t.Fatalf("pod = %+v", u)
	}
	if u := got[2]; u.CPUMillicores != 1 || u.MemoryBytes != 20<<20 {
		t.Fatalf("pod = %+v", u)
	}

	k = fakeAPIServer(t, true)
	if _, err := k.AppUsage(context.Background(), "blog"); !errors.Is(err, ErrNoMetricsAPI) {
		t.Fatalf("without metrics-server: %v", err)
	}
}
