package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nisagwn/paas/internal/store"
)

func newTraefikFake() *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		ingressRouteGVR:   "IngressRouteList",
		traefikServiceGVR: "TraefikServiceList",
	})
}

func testSplit(weight int) *store.TrafficSplit {
	return &store.TrafficSplit{
		RolloutID:    7,
		Stable:       store.SplitBackend{DeploymentID: 1, CommitSHA: strings.Repeat("a", 40)},
		Canary:       store.SplitBackend{DeploymentID: 2, CommitSHA: strings.Repeat("b", 40), Generation: 1},
		CanaryWeight: weight,
		Routes: []store.AliasRoute{
			{Hostname: "blog.paas.test", Kind: store.AliasProduction, DeploymentID: 1},
			{Hostname: "www.example.com", Kind: store.AliasCustom, DeploymentID: 1},
		},
	}
}

func weights(t *testing.T, ts *unstructured.Unstructured) map[string]int64 {
	t.Helper()
	svcs, _, _ := unstructured.NestedSlice(ts.Object, "spec", "weighted", "services")
	out := map[string]int64{}
	for _, s := range svcs {
		m := s.(map[string]any)
		if m["port"] != "http" {
			t.Fatalf("port = %v", m["port"])
		}
		out[m["name"].(string)] = m["weight"].(int64)
	}
	return out
}

func TestApplyRollout(t *testing.T) {
	k, _ := newDeployer(t, nil, time.Second)
	dyn := newTraefikFake()
	k.Traefik = dyn
	ctx := context.Background()
	routes, services := dyn.Resource(ingressRouteGVR).Namespace(ns), dyn.Resource(traefikServiceGVR).Namespace(ns)

	if err := k.ApplyRollout(ctx, "blog", testSplit(10)); err != nil {
		t.Fatal(err)
	}
	ts, err := services.Get(ctx, RolloutServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if w := weights(t, ts); w["d-aaaaaaa"] != 90 || w["d-bbbbbbb-1"] != 10 || len(w) != 2 {
		t.Fatalf("weights = %v", w)
	}
	prod, err := routes.Get(ctx, "alias-blog", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r0, _, _ := unstructured.NestedSlice(prod.Object, "spec", "routes")
	route := r0[0].(map[string]any)
	if route["match"] != "Host(`blog.paas.test`) && !PathPrefix(`/.well-known/acme-challenge/`)" ||
		route["priority"] != int64(RolloutPriority) {
		t.Fatalf("route = %v", route)
	}
	svc := route["services"].([]any)[0].(map[string]any)
	if svc["name"] != RolloutServiceName || svc["kind"] != "TraefikService" {
		t.Fatalf("route service = %v", svc)
	}
	// TLS as the alias Ingress: websecure + default certificate; the
	// custom domain uses its cert-manager Secret.
	if eps, _, _ := unstructured.NestedStringSlice(prod.Object, "spec", "entryPoints"); len(eps) != 1 || eps[0] != "websecure" {
		t.Fatalf("entry points = %v", eps)
	}
	if tls, ok, _ := unstructured.NestedMap(prod.Object, "spec", "tls"); !ok || len(tls) != 0 {
		t.Fatalf("production tls = %v", tls)
	}
	dom, err := routes.Get(ctx, domainIngressName("www.example.com"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s, _, _ := unstructured.NestedString(dom.Object, "spec", "tls", "secretName"); s != domainSecretName("www.example.com") {
		t.Fatalf("custom domain secret = %q", s)
	}
	if dom.GetLabels()[LabelRoute] != RouteRollout || dom.GetLabels()[LabelRolloutID] != "7" ||
		dom.GetLabels()[LabelAliasKind] != store.AliasCustom {
		t.Fatalf("labels = %v", dom.GetLabels())
	}

	// The next step only edits the weights in place.
	uid := ts.GetUID()
	if err := k.ApplyRollout(ctx, "blog", testSplit(50)); err != nil {
		t.Fatal(err)
	}
	ts, _ = services.Get(ctx, RolloutServiceName, metav1.GetOptions{})
	if w := weights(t, ts); w["d-aaaaaaa"] != 50 || w["d-bbbbbbb-1"] != 50 || ts.GetUID() != uid {
		t.Fatalf("after advance: %v", w)
	}
	// Idempotent: no update without a change.
	dyn.ClearActions()
	if err := k.ApplyRollout(ctx, "blog", testSplit(50)); err != nil {
		t.Fatal(err)
	}
	for _, a := range dyn.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" {
			t.Fatalf("unexpected %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}

	// A custom domain removed during the canary loses its overlay.
	sp := testSplit(50)
	sp.Routes = sp.Routes[:1]
	if err := k.ApplyRollout(ctx, "blog", sp); err != nil {
		t.Fatal(err)
	}
	if _, err := routes.Get(ctx, domainIngressName("www.example.com"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stale overlay kept: %v", err)
	}

	// The end of the rollout removes everything.
	if err := k.ApplyRollout(ctx, "blog", nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := routes.List(ctx, metav1.ListOptions{}); len(l.Items) != 0 {
		t.Fatalf("overlays left: %d", len(l.Items))
	}
	if _, err := services.Get(ctx, RolloutServiceName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("traefikservice left: %v", err)
	}
	if err := k.ApplyRollout(ctx, "blog", nil); err != nil {
		t.Fatalf("idempotent removal: %v", err)
	}
}

func TestApplyRolloutTLSVariants(t *testing.T) {
	cfgs := map[string]func(*Config){
		"plain http":  func(c *Config) { c.TLS = false },
		"cert issuer": func(c *Config) { c.CertIssuer = "letsencrypt" },
	}
	for name, mut := range cfgs {
		t.Run(name, func(t *testing.T) {
			k, _ := newDeployer(t, nil, time.Second)
			mut(&k.cfg)
			ir := k.rolloutRoute(ns, "blog", testSplit(10), store.AliasRoute{Hostname: "blog.paas.test", Kind: store.AliasProduction})
			eps, _, _ := unstructured.NestedStringSlice(ir.Object, "spec", "entryPoints")
			secret, _, _ := unstructured.NestedString(ir.Object, "spec", "tls", "secretName")
			_, hasTLS := ir.Object["spec"].(map[string]any)["tls"]
			switch name {
			case "plain http":
				if eps[0] != "web" || hasTLS {
					t.Fatalf("plain: %v %v", eps, ir.Object["spec"])
				}
			case "cert issuer":
				// Same Secret as the alias Ingress's per-host certificate.
				if eps[0] != "websecure" || secret != certSecretName("alias-blog") {
					t.Fatalf("issuer: %v %q", eps, secret)
				}
			}
		})
	}
}

func TestApplyRolloutWithoutCRDs(t *testing.T) {
	k, _ := newDeployer(t, nil, time.Second)
	// Without a dynamic client nothing can split.
	if err := k.ApplyRollout(context.Background(), "blog", testSplit(10)); !errors.Is(err, ErrTraefikCRD) {
		t.Fatalf("no client: %v", err)
	}
	if err := k.ApplyRollout(context.Background(), "blog", nil); err != nil {
		t.Fatalf("no client, no split: %v", err)
	}
	// The API server answers 404 for an unknown resource type.
	dyn := newTraefikFake()
	notFound := func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
	}
	dyn.PrependReactor("*", "*", notFound)
	k.Traefik = dyn
	if err := k.ApplyRollout(context.Background(), "blog", testSplit(10)); !errors.Is(err, ErrTraefikCRD) {
		t.Fatalf("missing CRD: %v", err)
	}
	if err := k.ApplyRollout(context.Background(), "blog", nil); err != nil {
		t.Fatalf("missing CRD, no split: %v", err)
	}
}

// Exposition captured from Traefik 3.6 (k3s) during a live canary at 20 %
// (f21-test namespace): the weighted TraefikService exports only its
// children, named <ns>-<service>-<port>@kubernetescrd, while the alias
// Ingress's backend keeps its @kubernetes series. The CRD series stay
// after the overlay is removed.
const canaryExposition = `# HELP traefik_service_requests_total How many HTTP requests processed on a service, partitioned by status code, protocol, and method.
# TYPE traefik_service_requests_total counter
traefik_service_requests_total{code="200",method="GET",protocol="http",service="app-blog-d-aaaaaaa-http@kubernetes"} 20
traefik_service_requests_total{code="200",method="GET",protocol="http",service="app-blog-d-aaaaaaa-http@kubernetescrd"} 160
traefik_service_requests_total{code="503",method="GET",protocol="http",service="app-blog-d-bbbbbbb-1-http@kubernetescrd"} 4
traefik_service_requests_total{code="200",method="GET",protocol="http",service="app-blog-d-bbbbbbb-1-http@kubernetescrd"} 36
traefik_service_request_duration_seconds_bucket{code="200",method="GET",protocol="http",service="app-blog-d-bbbbbbb-1-http@kubernetescrd",le="0.1"} 30
traefik_service_request_duration_seconds_bucket{code="200",method="GET",protocol="http",service="app-blog-d-bbbbbbb-1-http@kubernetescrd",le="+Inf"} 36
traefik_service_request_duration_seconds_sum{code="200",method="GET",protocol="http",service="app-blog-d-bbbbbbb-1-http@kubernetescrd"} 1.2
traefik_service_request_duration_seconds_count{code="200",method="GET",protocol="http",service="app-blog-d-bbbbbbb-1-http@kubernetescrd"} 36
traefik_service_requests_total{code="200",method="GET",protocol="http",service="dashboard@internal"} 3
`

func TestCanaryMetricKeys(t *testing.T) {
	stats, err := ParseServiceStats([]byte(canaryExposition))
	if err != nil {
		t.Fatal(err)
	}
	canary := stats[CRDMetricKey(ns, "d-bbbbbbb-1")]
	if canary.Requests != 40 || canary.Classes[3] != 4 || canary.Buckets[0.1] != 30 || canary.DurationCount != 36 {
		t.Fatalf("canary series = %+v", canary)
	}
	if stats[MetricKey(ns, "d-aaaaaaa")].Requests != 20 || stats[CRDMetricKey(ns, "d-aaaaaaa")].Requests != 160 {
		t.Fatalf("stable series = %+v", stats)
	}
	// Idle detection sees one total per deployment.
	if IngressMetricKey(CRDMetricKey(ns, "d-aaaaaaa")) != MetricKey(ns, "d-aaaaaaa") ||
		IngressMetricKey("dashboard@internal") != "dashboard@internal" {
		t.Fatal("IngressMetricKey")
	}
}
