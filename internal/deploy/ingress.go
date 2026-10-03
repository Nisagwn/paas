package deploy

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nisagwn/minipaas/internal/naming"
	"github.com/nisagwn/minipaas/internal/store"
)

// Routing uses plain networking.k8s.io/v1 Ingress objects. Traefik (bundled
// with k3s) serves them, but any ingress controller would; no CRDs needed.
// TLS: an Ingress lists its host under spec.tls without a secretName, so
// Traefik answers with its default certificate, the wildcard *.domain.
const (
	LabelRoute     = "minipaas/route" // "deployment" or "alias"
	LabelAliasKind = "minipaas/alias-kind"
	RouteDeploy    = "deployment"
	RouteAlias     = "alias"

	annotEntryPoints = "traefik.ingress.kubernetes.io/router.entrypoints"
	annotRouterTLS   = "traefik.ingress.kubernetes.io/router.tls"
)

// ingressObject routes host to the Service of the deployment with commit sha.
func (k *Kubernetes) ingressObject(ns, name, host, sha string, lbl map[string]string) *networkingv1.Ingress {
	annot := map[string]string{annotEntryPoints: "web"}
	var tls []networkingv1.IngressTLS
	if k.cfg.TLS {
		annot = map[string]string{annotEntryPoints: "websecure", annotRouterTLS: "true"}
		tls = []networkingv1.IngressTLS{{Hosts: []string{host}}}
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: lbl, Annotations: annot},
		Spec: networkingv1.IngressSpec{
			TLS: tls,
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path:     "/",
						PathType: ptr(networkingv1.PathTypePrefix),
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: naming.ResourceName(sha),
							Port: networkingv1.ServiceBackendPort{Name: "http"},
						}},
					}},
				}},
			}},
		},
	}
	if k.cfg.IngressClass != "" {
		ing.Spec.IngressClassName = ptr(k.cfg.IngressClass)
	}
	return ing
}

func (k *Kubernetes) applyIngress(ctx context.Context, want *networkingv1.Ingress) (*networkingv1.Ingress, error) {
	return apply(ctx, k.client.NetworkingV1().Ingresses(want.Namespace), want.Name, want,
		func(have *networkingv1.Ingress) bool {
			if equality.Semantic.DeepEqual(have.Spec, want.Spec) &&
				equality.Semantic.DeepEqual(have.Labels, want.Labels) &&
				equality.Semantic.DeepEqual(have.Annotations, want.Annotations) {
				return false
			}
			have.Spec, have.Labels, have.Annotations = want.Spec, want.Labels, want.Annotations
			return true
		})
}

// ensureDeploymentIngress serves the immutable <sha7>-<app>.<domain> URL.
// The Deployment owns it, so deleting a deployment removes its route.
func (k *Kubernetes) ensureDeploymentIngress(ctx context.Context, d store.Deployment, dep *appsv1.Deployment) (string, error) {
	host := naming.DeploymentHost(d.CommitSHA, d.AppName, k.cfg.Domain)
	lbl := labels(d)
	lbl[LabelRoute] = RouteDeploy
	want := k.ingressObject(dep.Namespace, dep.Name, host, d.CommitSHA, lbl)
	want.OwnerReferences = []metav1.OwnerReference{ownerRef(dep)}
	_, err := k.applyIngress(ctx, want)
	return host, err
}

// aliasIngressName derives a stable object name from an alias hostname. The
// first DNS label is unique per app (<app> or <branch>-<app>) and ≤ 63 chars.
func aliasIngressName(hostname string) string {
	label, _, _ := strings.Cut(hostname, ".")
	return "alias-" + label
}

// ApplyAliases makes the cluster match the app's aliases: one Ingress per
// alias hostname pointing at the target deployment's Service, and no others.
// It is idempotent, so it is safe to call after every change and from a
// periodic reconcile loop. Moving an alias (rollback) only edits the
// backend of an existing Ingress: no pod restarts, no rebuild.
func (k *Kubernetes) ApplyAliases(ctx context.Context, app string, routes []store.AliasRoute) error {
	ns := naming.Namespace(app)
	keep := make(map[string]bool, len(routes))
	for _, r := range routes {
		name := aliasIngressName(r.Hostname)
		keep[name] = true
		lbl := map[string]string{
			LabelManagedBy:    ManagedBy,
			LabelApp:          app,
			LabelRoute:        RouteAlias,
			LabelAliasKind:    r.Kind,
			LabelDeploymentID: fmt.Sprint(r.DeploymentID),
			LabelCommit:       r.CommitSHA,
		}
		if _, err := k.applyIngress(ctx, k.ingressObject(ns, name, r.Hostname, r.CommitSHA, lbl)); err != nil {
			return fmt.Errorf("alias %s: %w", r.Hostname, err)
		}
	}

	// Remove aliases that no longer exist in the database.
	ic := k.client.NetworkingV1().Ingresses(ns)
	list, err := ic.List(ctx, metav1.ListOptions{LabelSelector: LabelRoute + "=" + RouteAlias})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, ing := range list.Items {
		if keep[ing.Name] {
			continue
		}
		err := ic.Delete(ctx, ing.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &ing.UID}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale alias %s: %w", ing.Name, err)
		}
	}
	return nil
}
