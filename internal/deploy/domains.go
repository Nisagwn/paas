package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// Custom domains (Faz 12) are not covered by the platform's wildcard
// certificate. Their Ingress names a per-domain TLS Secret and carries the
// cert-manager annotation, so cert-manager's ingress-shim creates a
// Certificate (named like the Secret) and solves HTTP-01 through Traefik.
const (
	AnnotClusterIssuer = "cert-manager.io/cluster-issuer"
	// DefaultCustomDomainIssuer is the HTTP-01 ClusterIssuer in infra/k8s/40-tls.yaml.
	DefaultCustomDomainIssuer = "letsencrypt-http01"
)

var certificateGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}

// domainIngressName is a readable, unique object name for a custom domain:
// "domain-" + the hostname with dots as dashes (truncated) + a short hash.
func domainIngressName(host string) string {
	sum := sha256.Sum256([]byte(host))
	label := strings.Trim(naming.Slug(host), "-")
	if len(label) > 42 { // "-tls" Secret name ≤ 63: cert-manager uses it as a label value
		label = strings.TrimRight(label[:42], "-")
	}
	return "domain-" + label + "-" + hex.EncodeToString(sum[:4])
}

// domainSecretName holds the custom domain's certificate.
func domainSecretName(host string) string { return domainIngressName(host) + "-tls" }

// customDomainTLS switches a route Ingress from the default (wildcard)
// certificate to a per-domain one issued by cert-manager. Without TLS the
// Ingress stays plain HTTP.
func (k *Kubernetes) customDomainTLS(ing *networkingv1.Ingress) {
	if !k.cfg.TLS {
		return
	}
	host := ing.Spec.Rules[0].Host
	ing.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{host}, SecretName: domainSecretName(host)}}
	issuer := k.cfg.CustomDomainIssuer
	if issuer == "" {
		issuer = DefaultCustomDomainIssuer
	}
	ing.Annotations[AnnotClusterIssuer] = issuer
}

// deleteDomainSecret removes the certificate Secret of a deleted custom
// domain Ingress; cert-manager deletes the Certificate with the Ingress
// but leaves the Secret behind.
func (k *Kubernetes) deleteDomainSecret(ctx context.Context, ing *networkingv1.Ingress) error {
	if ing.Labels[LabelAliasKind] != store.AliasCustom {
		return nil
	}
	for _, t := range ing.Spec.TLS {
		if t.SecretName == "" {
			continue
		}
		err := k.client.CoreV1().Secrets(ing.Namespace).Delete(ctx, t.SecretName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// DomainCertificate reports whether the certificate of a custom domain is
// issued. Without TLS there is nothing to issue; without a dynamic client
// (Certificates unset) the status is unknown and reported as ready.
func (k *Kubernetes) DomainCertificate(ctx context.Context, app, host string) (bool, string, error) {
	if !k.cfg.TLS {
		return true, "", nil
	}
	if k.Certificates == nil {
		return true, "", nil
	}
	cert, err := k.Certificates.Resource(certificateGVR).Namespace(naming.Namespace(app)).
		Get(ctx, domainSecretName(host), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, "certificate not created yet", nil
	}
	if err != nil {
		return false, "", err
	}
	conds, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] != "Ready" {
			continue
		}
		if m["status"] == "True" {
			return true, "", nil
		}
		return false, fmt.Sprint(m["message"]), nil
	}
	return false, "certificate is being issued", nil
}

// NewDynamicClient returns a dynamic client with NewClient's configuration,
// used to read cert-manager Certificates.
func NewDynamicClient(kubeconfig string) (dynamic.Interface, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(cfg)
}
