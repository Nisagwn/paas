package deploy

// Faz 21: weighted routing for canary rollouts.
//
// A networking/v1 Ingress names exactly one Service per path, and
// Traefik's Ingress provider does not accept resource backends ("Resource
// backends are not supported", checked on Traefik 3.6), so it cannot split
// traffic. While a canary runs, the production hostnames therefore get an
// overlay from Traefik's CRD provider, next to the unchanged alias Ingresses:
//
//	TraefikService rollout       weighted: d-<stable> (100-w) + d-<canary> (w)
//	IngressRoute   alias-<app>   Host(<app>.<domain>)  → TraefikService rollout
//	IngressRoute   domain-<...>  Host(custom domain)   → TraefikService rollout
//
// The IngressRoutes carry a priority far above the Ingress routers of the
// same host (Traefik's default priority is the rule length), so they win
// while they exist; when the rollout ends they are deleted and the alias
// Ingress (by then pointing at the promoted or the old deployment) serves
// the host again. Nothing is ever without a route: the overlay is created
// after the TraefikService and removed before it, and the Ingress stays
// all along. ACME HTTP-01 challenges are excluded from the overlay so
// cert-manager keeps renewing custom domain certificates during a canary.
// TLS is the alias Ingress's: the websecure entry point with the default
// (wildcard) certificate, or the per-host Secret of CertIssuer / custom
// domains (the Ingress keeps its cert-manager annotation, so the Secret
// keeps being issued).
//
// Metrics: Traefik names the children of a weighted TraefikService like the
// Ingress provider names its backends, with the CRD provider's suffix:
// <ns>-<service>-<port>@kubernetescrd (see CRDMetricKey), so a canary's
// requests are attributed to the right deployment by analytics and scale
// to zero. The weighted parent (<ns>-rollout@kubernetescrd) belongs to no
// deployment and is ignored.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

const (
	// RouteRollout labels the IngressRoutes of a canary (LabelRoute).
	RouteRollout = "rollout"
	// LabelRolloutID is the rollout an overlay belongs to.
	LabelRolloutID = "paas/rollout-id"
	// RolloutServiceName is the weighted TraefikService of an app's canary.
	RolloutServiceName = "rollout"
	// RolloutPriority puts the overlay above the alias Ingress's router.
	RolloutPriority     = 100000
	acmeChallengePrefix = "/.well-known/acme-challenge/"
)

var (
	ingressRouteGVR   = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}
	traefikServiceGVR = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "traefikservices"}
)

// ErrTraefikCRD means the cluster has no Traefik CRDs (or the control plane
// may not use them): a canary cannot split traffic.
var ErrTraefikCRD = errors.New("traefik CRDs (traefik.io/v1alpha1 IngressRoute, TraefikService) are not available")

// CRDMetricKey is how Traefik names a Kubernetes Service referenced from a
// TraefikService or IngressRoute (port "http") in its metrics.
func CRDMetricKey(ns, svc string) string { return ns + "-" + svc + "-http@kubernetescrd" }

// dynamicClient returns the client for Traefik's CRDs.
func (k *Kubernetes) dynamicClient() dynamic.Interface {
	if k.Traefik != nil {
		return k.Traefik
	}
	return k.Certificates // NewDynamicClient: the same generic client
}

// ApplyRollout makes the app's canary overlay match split; nil removes it.
// It is idempotent (routing.Syncer calls it after every ApplyAliases).
func (k *Kubernetes) ApplyRollout(ctx context.Context, app string, split *store.TrafficSplit) error {
	ns := naming.Namespace(app)
	dc := k.dynamicClient()
	if dc == nil {
		if split == nil {
			return nil
		}
		return fmt.Errorf("%w: no dynamic client", ErrTraefikCRD)
	}
	routes, services := dc.Resource(ingressRouteGVR).Namespace(ns), dc.Resource(traefikServiceGVR).Namespace(ns)

	keep := map[string]bool{}
	if split != nil {
		if err := applyObject(ctx, services, k.rolloutService(ns, app, split)); err != nil {
			return fmt.Errorf("traefikservice %s/%s: %w", ns, RolloutServiceName, err)
		}
		for _, r := range split.Routes {
			want := k.rolloutRoute(ns, app, split, r)
			keep[want.GetName()] = true
			if err := applyObject(ctx, routes, want); err != nil {
				return fmt.Errorf("ingressroute %s/%s: %w", ns, want.GetName(), err)
			}
		}
	}

	list, err := routes.List(ctx, metav1.ListOptions{LabelSelector: LabelRoute + "=" + RouteRollout})
	if apierrors.IsNotFound(err) && split == nil {
		return nil // no CRDs: nothing to remove
	}
	if err != nil {
		return fmt.Errorf("list ingressroutes: %w", err)
	}
	for _, ir := range list.Items {
		if keep[ir.GetName()] {
			continue
		}
		uid := ir.GetUID()
		err := routes.Delete(ctx, ir.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ingressroute %s: %w", ir.GetName(), err)
		}
	}
	if split == nil {
		err := services.Delete(ctx, RolloutServiceName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete traefikservice %s: %w", RolloutServiceName, err)
		}
	}
	return nil
}

func rolloutLabels(app string, split *store.TrafficSplit) map[string]any {
	return map[string]any{
		LabelManagedBy: ManagedBy,
		LabelApp:       app,
		LabelRoute:     RouteRollout,
		LabelRolloutID: strconv.FormatInt(split.RolloutID, 10),
	}
}

// rolloutService is the weighted TraefikService of the split.
func (k *Kubernetes) rolloutService(ns, app string, split *store.TrafficSplit) *unstructured.Unstructured {
	w := int64(min(max(split.CanaryWeight, 0), 100))
	backend := func(b store.SplitBackend, weight int64) map[string]any {
		return map[string]any{
			"name": naming.ObjectName(b.CommitSHA, b.Generation), "port": "http", "weight": weight,
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "TraefikService",
		"metadata": map[string]any{
			"name": RolloutServiceName, "namespace": ns, "labels": rolloutLabels(app, split),
		},
		"spec": map[string]any{
			"weighted": map[string]any{
				"services": []any{backend(split.Stable, 100-w), backend(split.Canary, w)},
			},
		},
	}}
}

// rolloutRoute is the IngressRoute overlay of one production hostname,
// with the TLS of its alias Ingress (ingressObject, customDomainTLS).
func (k *Kubernetes) rolloutRoute(ns, app string, split *store.TrafficSplit, r store.AliasRoute) *unstructured.Unstructured {
	name := aliasIngressName(r.Hostname)
	if r.Kind == store.AliasCustom {
		name = domainIngressName(r.Hostname)
	}
	lbl := rolloutLabels(app, split)
	lbl[LabelAliasKind] = r.Kind
	spec := map[string]any{
		"entryPoints": []any{"web"},
		"routes": []any{map[string]any{
			"kind":     "Rule",
			"match":    fmt.Sprintf("Host(`%s`) && !PathPrefix(`%s`)", r.Hostname, acmeChallengePrefix),
			"priority": int64(RolloutPriority),
			"services": []any{map[string]any{"name": RolloutServiceName, "kind": "TraefikService"}},
		}},
	}
	if k.cfg.TLS {
		spec["entryPoints"] = []any{"websecure"}
		tls := map[string]any{} // the default (wildcard) certificate
		switch {
		case r.Kind == store.AliasCustom:
			tls["secretName"] = domainSecretName(r.Hostname)
		case k.cfg.CertIssuer != "":
			tls["secretName"] = certSecretName(name)
		}
		spec["tls"] = tls
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "IngressRoute",
		"metadata":   map[string]any{"name": name, "namespace": ns, "labels": lbl},
		"spec":       spec,
	}}
}

// applyObject creates want or updates its labels and spec in place.
func applyObject(ctx context.Context, ri dynamic.ResourceInterface, want *unstructured.Unstructured) error {
	have, err := ri.Get(ctx, want.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = ri.Create(ctx, want, metav1.CreateOptions{})
		if apierrors.IsNotFound(err) {
			// The namespace exists (the app has deployments), so the
			// resource type itself is unknown.
			return fmt.Errorf("%w: %v", ErrTraefikCRD, err)
		}
		return err
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(have.Object["spec"], want.Object["spec"]) &&
		equality.Semantic.DeepEqual(have.GetLabels(), want.GetLabels()) {
		return nil
	}
	have.Object["spec"] = want.Object["spec"]
	have.SetLabels(want.GetLabels())
	_, err = ri.Update(ctx, have, metav1.UpdateOptions{})
	return err
}
