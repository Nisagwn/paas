package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nisagwn/minipaas/internal/store"
)

func backend(ing *networkingv1.Ingress) string {
	return ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name
}

func TestDeployCreatesIngress(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	becomeAvailable(t, cs)
	var l logs
	if err := k.Deploy(context.Background(), dep, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}

	ing, err := cs.NetworkingV1().Ingresses(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	host := "a3f9c1d-blog.paas.test"
	if r := ing.Spec.Rules[0]; r.Host != host || backend(ing) != name ||
		r.HTTP.Paths[0].Backend.Service.Port.Name != "http" {
		t.Fatalf("rule = %+v", r)
	}
	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "traefik" {
		t.Errorf("ingress class = %v", ing.Spec.IngressClassName)
	}
	// TLS with no secretName: Traefik's default (wildcard) certificate.
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].Hosts[0] != host || ing.Spec.TLS[0].SecretName != "" {
		t.Errorf("tls = %+v", ing.Spec.TLS)
	}
	if ing.Annotations[annotEntryPoints] != "websecure" || ing.Annotations[annotRouterTLS] != "true" {
		t.Errorf("annotations = %v", ing.Annotations)
	}
	if ing.Labels[LabelRoute] != RouteDeploy || len(ing.OwnerReferences) != 1 || ing.OwnerReferences[0].Name != name {
		t.Errorf("labels/owner = %v / %+v", ing.Labels, ing.OwnerReferences)
	}
	if !strings.Contains(l.String(), "ingress app-blog/d-a3f9c1d → https://"+host) {
		t.Errorf("log lacks the ingress line:\n%s", &l)
	}
}

func TestIngressWithoutTLS(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Domain, cfg.TLS, cfg.IngressClass = "localtest.me", false, ""
	k, err := New(fake.NewClientset(), nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ing := k.ingressObject(ns, "x", "blog.localtest.me", sha, nil)
	if len(ing.Spec.TLS) != 0 || ing.Annotations[annotEntryPoints] != "web" || ing.Spec.IngressClassName != nil {
		t.Fatalf("got tls=%v annotations=%v class=%v", ing.Spec.TLS, ing.Annotations, ing.Spec.IngressClassName)
	}
}

func TestApplyAliases(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	ctx := context.Background()
	ic := cs.NetworkingV1().Ingresses(ns)
	shaA, shaB := strings.Repeat("a", 40), strings.Repeat("b", 40)

	prod := store.AliasRoute{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main", DeploymentID: 1, CommitSHA: shaA}
	preview := store.AliasRoute{Hostname: "feature-x-blog.paas.test", Kind: store.AliasPreview, Branch: "feature/x", DeploymentID: 2, CommitSHA: shaB}
	if err := k.ApplyAliases(ctx, "blog", []store.AliasRoute{prod, preview}); err != nil {
		t.Fatal(err)
	}
	p, err := ic.Get(ctx, "alias-blog", metav1.GetOptions{})
	if err != nil || backend(p) != "d-aaaaaaa" || p.Spec.Rules[0].Host != "blog.paas.test" ||
		p.Labels[LabelAliasKind] != store.AliasProduction || len(p.OwnerReferences) != 0 {
		t.Fatalf("production alias: %+v %v", p, err)
	}
	if v, err := ic.Get(ctx, "alias-feature-x-blog", metav1.GetOptions{}); err != nil || backend(v) != "d-bbbbbbb" {
		t.Fatalf("preview alias: %v", err)
	}

	// Rollback: production now points at deployment B. Only the backend moves.
	prod.DeploymentID, prod.CommitSHA = 2, shaB
	if err := k.ApplyAliases(ctx, "blog", []store.AliasRoute{prod, preview}); err != nil {
		t.Fatal(err)
	}
	p2, _ := ic.Get(ctx, "alias-blog", metav1.GetOptions{})
	if backend(p2) != "d-bbbbbbb" || p2.UID != p.UID || p2.Labels[LabelDeploymentID] != "2" {
		t.Fatalf("after rollback: backend=%s same object=%v labels=%v", backend(p2), p2.UID == p.UID, p2.Labels)
	}

	// An alias gone from the database is removed; deployment routes stay.
	ic.Create(ctx, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns, Labels: map[string]string{LabelRoute: RouteDeploy}}}, metav1.CreateOptions{})
	if err := k.ApplyAliases(ctx, "blog", []store.AliasRoute{prod}); err != nil {
		t.Fatal(err)
	}
	list, _ := ic.List(ctx, metav1.ListOptions{})
	var names []string
	for _, i := range list.Items {
		names = append(names, i.Name)
	}
	if strings.Join(names, ",") != "alias-blog,"+name {
		t.Fatalf("ingresses = %v, want alias-blog and %s", names, name)
	}

	// Re-applying the same state writes nothing.
	cs.ClearActions()
	k.ApplyAliases(ctx, "blog", []store.AliasRoute{prod})
	for _, a := range cs.Actions() {
		if v := a.GetVerb(); v != "get" && v != "list" {
			t.Fatalf("unchanged state caused a %s", v)
		}
	}
}

func TestApplyAliasesNoNamespace(t *testing.T) {
	k, _ := newDeployer(t, nil, time.Second)
	if err := k.ApplyAliases(context.Background(), "ghost", nil); err != nil {
		t.Fatalf("app without deployments: %v", err)
	}
}
