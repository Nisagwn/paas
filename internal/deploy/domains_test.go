package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nisagwn/paas/internal/store"
)

func TestDomainIngressName(t *testing.T) {
	a, b := domainIngressName("www.example.com"), domainIngressName("www-example.com")
	if !strings.HasPrefix(a, "domain-www-example-com-") || a == b {
		t.Fatalf("names %q %q", a, b)
	}
	long := domainSecretName(strings.Repeat("abcdefghi.", 20) + "com")
	if len(long) > 63 {
		t.Fatalf("name too long: %q", long)
	}
}

func TestApplyAliasesCustomDomain(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	ctx := context.Background()
	ic := cs.NetworkingV1().Ingresses(ns)
	shaA, shaB := strings.Repeat("a", 40), strings.Repeat("b", 40)

	prod := store.AliasRoute{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main", DeploymentID: 2, CommitSHA: shaB}
	custom := store.AliasRoute{Hostname: "www.example.com", Kind: store.AliasCustom, Branch: "main", DeploymentID: 2, CommitSHA: shaB}
	if err := k.ApplyAliases(ctx, "blog", []store.AliasRoute{prod, custom}); err != nil {
		t.Fatal(err)
	}
	name := domainIngressName("www.example.com")
	ing, err := ic.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if backend(ing) != "d-bbbbbbb" || ing.Spec.Rules[0].Host != "www.example.com" || ing.Labels[LabelAliasKind] != store.AliasCustom {
		t.Fatalf("custom ingress: %+v", ing)
	}
	// Its own certificate: a per-domain Secret and the HTTP-01 issuer.
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != name+"-tls" || ing.Spec.TLS[0].Hosts[0] != "www.example.com" {
		t.Fatalf("tls = %+v", ing.Spec.TLS)
	}
	if ing.Annotations[AnnotClusterIssuer] != DefaultCustomDomainIssuer || ing.Annotations[annotEntryPoints] != "websecure" {
		t.Fatalf("annotations = %v", ing.Annotations)
	}
	// The platform alias keeps the wildcard certificate.
	if p, _ := ic.Get(ctx, "alias-blog", metav1.GetOptions{}); p.Spec.TLS[0].SecretName != "" || p.Annotations[AnnotClusterIssuer] != "" {
		t.Fatalf("production alias: %+v", p)
	}

	// Rollback moves the custom domain's backend in place.
	prod.DeploymentID, prod.CommitSHA = 1, shaA
	custom.DeploymentID, custom.CommitSHA = 1, shaA
	if err := k.ApplyAliases(ctx, "blog", []store.AliasRoute{prod, custom}); err != nil {
		t.Fatal(err)
	}
	ing2, _ := ic.Get(ctx, name, metav1.GetOptions{})
	if backend(ing2) != "d-aaaaaaa" || ing2.UID != ing.UID {
		t.Fatalf("after rollback: %s", backend(ing2))
	}

	// Removing the domain deletes its Ingress and the certificate Secret.
	cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-tls", Namespace: ns}}, metav1.CreateOptions{})
	if err := k.ApplyAliases(ctx, "blog", []store.AliasRoute{prod}); err != nil {
		t.Fatal(err)
	}
	if _, err := ic.Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Fatal("custom domain ingress not removed")
	}
	if _, err := cs.CoreV1().Secrets(ns).Get(ctx, name+"-tls", metav1.GetOptions{}); err == nil {
		t.Fatal("certificate secret not removed")
	}
}

func TestCustomDomainWithoutTLS(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Domain, cfg.TLS, cfg.CustomDomainIssuer = "localtest.me", false, "my-issuer"
	k, err := New(fake.NewClientset(), nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := store.AliasRoute{Hostname: "www.example.com", Kind: store.AliasCustom, CommitSHA: sha}
	if err := k.ApplyAliases(context.Background(), "blog", []store.AliasRoute{r}); err != nil {
		t.Fatal(err)
	}
	ing, _ := k.client.NetworkingV1().Ingresses(ns).Get(context.Background(), domainIngressName(r.Hostname), metav1.GetOptions{})
	if len(ing.Spec.TLS) != 0 || ing.Annotations[AnnotClusterIssuer] != "" || ing.Annotations[annotEntryPoints] != "web" {
		t.Fatalf("plain http ingress: tls=%v annotations=%v", ing.Spec.TLS, ing.Annotations)
	}
	if ok, _, _ := k.DomainCertificate(context.Background(), "blog", r.Hostname); !ok {
		t.Fatal("without TLS there is no certificate to wait for")
	}

	cfg.TLS = true
	k, _ = New(fake.NewClientset(), nil, cfg)
	ing = k.ingressObject(ns, "x", r.Hostname, sha, nil)
	k.customDomainTLS(ing)
	if ing.Annotations[AnnotClusterIssuer] != "my-issuer" {
		t.Fatalf("issuer = %v", ing.Annotations)
	}
}

func TestDomainCertificate(t *testing.T) {
	k, _ := newDeployer(t, nil, time.Second)
	host := "www.example.com"
	cert := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": domainSecretName(host), "namespace": ns},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "message": "Issuing certificate as Secret does not exist"},
		}},
	}}
	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{certificateGVR: "CertificateList"})
	k.Certificates = dyn
	ctx := context.Background()

	if ok, msg, err := k.DomainCertificate(ctx, "blog", host); ok || err != nil || msg != "certificate not created yet" {
		t.Fatalf("missing: %v %q %v", ok, msg, err)
	}
	if _, err := dyn.Resource(certificateGVR).Namespace(ns).Create(ctx, cert, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, msg, _ := k.DomainCertificate(ctx, "blog", host); ok || !strings.Contains(msg, "Issuing") {
		t.Fatalf("issuing: %v %q", ok, msg)
	}
	unstructured.SetNestedSlice(cert.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
	dyn.Resource(certificateGVR).Namespace(ns).Update(ctx, cert, metav1.UpdateOptions{})
	if ok, _, _ := k.DomainCertificate(ctx, "blog", host); !ok {
		t.Fatal("ready certificate reported as not ready")
	}
}
