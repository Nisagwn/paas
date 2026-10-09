package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/nisagwn/paas/internal/store"
)

type fakeProtection struct{ p store.Protection }

func (f *fakeProtection) ProtectionByAppName(_ context.Context, app string) (store.Protection, error) {
	if app != "blog" {
		return store.Protection{}, store.ErrNotFound
	}
	return f.p, nil
}

func newProtectFake() *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		ingressRouteGVR:   "IngressRouteList",
		traefikServiceGVR: "TraefikServiceList",
		middlewareGVR:     "MiddlewareList",
	})
}

// TestProtectionAnnotations: the middleware follows mode and scope on
// deployment, preview, production and custom domain Ingresses, and is
// removed (with the Middleware) when protection is turned off.
func TestProtectionAnnotations(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	dyn := newProtectFake()
	k.Traefik = dyn
	src := &fakeProtection{p: store.Protection{Mode: store.ProtectOff, Scope: store.ScopePreviews}}
	k.Protection = src
	k.cfg.ForwardAuthURL = "http://paas.paas.svc.cluster.local/_auth/verify"
	ctx := context.Background()
	becomeAvailable(t, cs)
	var l logs
	if err := k.Deploy(ctx, dep, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	shaA := strings.Repeat("a", 40)
	routes := []store.AliasRoute{
		{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main", DeploymentID: 1, CommitSHA: shaA},
		{Hostname: "feature-blog.paas.test", Kind: store.AliasPreview, Branch: "feature", DeploymentID: 1, CommitSHA: shaA},
		{Hostname: "www.example.com", Kind: store.AliasCustom, Branch: "main", DeploymentID: 1, CommitSHA: shaA},
	}
	ic := cs.NetworkingV1().Ingresses(ns)
	mws := dyn.Resource(middlewareGVR).Namespace(ns)
	sync := func() {
		t.Helper()
		if err := k.ApplyAliases(ctx, "blog", routes); err != nil {
			t.Fatal(err)
		}
		if err := k.ApplyProtection(ctx, "blog"); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want map[string]bool) {
		t.Helper()
		for ingName, protected := range want {
			ing, err := ic.Get(ctx, ingName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got, has := ing.Annotations[AnnotMiddlewares]
			if protected != has || (has && got != "app-blog-paas-protect@kubernetescrd") {
				t.Errorf("%s: middlewares = %q (protected %v)", ingName, got, protected)
			}
		}
	}
	prodIng, prevIng, domIng := aliasIngressName("blog.paas.test"), aliasIngressName("feature-blog.paas.test"), domainIngressName("www.example.com")

	sync()
	check(map[string]bool{name: false, prodIng: false, prevIng: false, domIng: false})
	if _, err := mws.Get(ctx, ProtectMiddleware, metav1.GetOptions{}); err == nil {
		t.Fatal("middleware exists while protection is off")
	}

	src.p = store.Protection{Mode: store.ProtectTeam, Scope: store.ScopePreviews, Version: 1}
	sync()
	check(map[string]bool{name: true, prodIng: false, prevIng: true, domIng: false})
	mw, err := mws.Get(ctx, ProtectMiddleware, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	addr, _, _ := unstructured.NestedString(mw.Object, "spec", "forwardAuth", "address")
	if addr != "http://paas.paas.svc.cluster.local/_auth/verify?app=blog" {
		t.Errorf("address = %q", addr)
	}
	if trust, _, _ := unstructured.NestedBool(mw.Object, "spec", "forwardAuth", "trustForwardHeader"); trust {
		t.Error("trustForwardHeader must be false")
	}

	src.p = store.Protection{Mode: store.ProtectPassword, Scope: store.ScopeAll, Version: 2}
	sync()
	check(map[string]bool{name: true, prodIng: true, prevIng: true, domIng: true})

	// A canary overlay of production carries the middleware with scope all.
	split := testSplit(10)
	if err := k.ApplyRollout(ctx, "blog", split); err != nil {
		t.Fatal(err)
	}
	ir, err := dyn.Resource(ingressRouteGVR).Namespace(ns).Get(ctx, aliasIngressName("blog.paas.test"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rs, _, _ := unstructured.NestedSlice(ir.Object, "spec", "routes")
	if m := rs[0].(map[string]any); m["middlewares"] == nil {
		t.Errorf("canary route without middleware: %v", m)
	}

	src.p = store.Protection{Mode: store.ProtectOff, Scope: store.ScopeAll, Version: 3}
	sync()
	if err := k.ApplyRollout(ctx, "blog", split); err != nil {
		t.Fatal(err)
	}
	check(map[string]bool{name: false, prodIng: false, prevIng: false, domIng: false})
	if _, err := mws.Get(ctx, ProtectMiddleware, metav1.GetOptions{}); err == nil {
		t.Fatal("middleware left behind after turning protection off")
	}
	ir, _ = dyn.Resource(ingressRouteGVR).Namespace(ns).Get(ctx, aliasIngressName("blog.paas.test"), metav1.GetOptions{})
	rs, _, _ = unstructured.NestedSlice(ir.Object, "spec", "routes")
	if m := rs[0].(map[string]any); m["middlewares"] != nil {
		t.Errorf("canary route keeps middleware: %v", m)
	}
}

// A deployment created while protection is on is protected from its first
// Ingress write; without a dynamic client enabling protection fails
// instead of leaving the route open.
func TestProtectionOnDeployAndWithoutCRDs(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	k.Protection = &fakeProtection{p: store.Protection{Mode: store.ProtectTeam, Scope: store.ScopePreviews, Version: 1}}
	becomeAvailable(t, cs)
	var l logs
	if err := k.Deploy(context.Background(), dep, image, l.log); err == nil {
		t.Fatal("deploy succeeded without Traefik CRDs while protection is on")
	}
	k.Traefik = newProtectFake()
	if err := k.Deploy(context.Background(), dep, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	ing, err := cs.NetworkingV1().Ingresses(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ing.Annotations[AnnotMiddlewares] != MiddlewareRef(ns) {
		t.Fatalf("annotations = %v", ing.Annotations)
	}
}
