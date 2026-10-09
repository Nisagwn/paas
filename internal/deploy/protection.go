package deploy

// Faz 24: deployment protection. A protected app gets one Traefik
// Middleware in its namespace,
//
//	Middleware paas-protect   forwardAuth → <ForwardAuthURL>?app=<app>
//
// and every Ingress in the protection's scope references it through the
// router.middlewares annotation (<ns>-paas-protect@kubernetescrd):
//
//	Ingress d-<sha7>          deployment URL         mode team / password
//	Ingress alias-<branch>-…  preview alias          mode team / password
//	Ingress alias-<app>       production alias       + scope all
//	Ingress domain-…          custom domain          + scope all
//	IngressRoute (canary)     production overlay     + scope all
//
// The annotation is computed in applyIngress, so every writer of a route
// Ingress (Deploy, ApplyAliases) produces the protected form and the
// periodic reconcile never strips it, not even briefly. ApplyProtection
// (routing.Syncer, after a settings change) rewrites the annotations of
// existing deployment Ingresses, which nothing else touches after the
// deploy, and removes the Middleware once nothing is protected. Turning
// protection on creates the Middleware before the first Ingress names it;
// turning it off removes the references first.
//
// The control plane answers the ForwardAuth subrequests (internal/protect).
// Without Traefik's CRDs protection cannot be enforced: enabling it then
// fails the route update instead of leaving the routes open.

import (
	"context"
	"errors"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

const (
	// ProtectMiddleware is the ForwardAuth Middleware of a protected app.
	ProtectMiddleware = "paas-protect"
	// AnnotMiddlewares lists the Traefik middlewares of an Ingress router.
	AnnotMiddlewares = "traefik.ingress.kubernetes.io/router.middlewares"
	// DefaultForwardAuthURL is the control plane's verify endpoint inside
	// the cluster (infra/k8s/31-control-plane.yaml, Service paas/paas).
	DefaultForwardAuthURL = "http://paas.paas.svc.cluster.local/_auth/verify"
)

var middlewareGVR = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "middlewares"}

// ProtectionSource reads an app's protection setting (store.Store).
type ProtectionSource interface {
	ProtectionByAppName(ctx context.Context, app string) (store.Protection, error)
}

// MiddlewareRef is how an Ingress in ns names the app's Middleware.
func MiddlewareRef(ns string) string { return ns + "-" + ProtectMiddleware + "@kubernetescrd" }

// protection returns app's setting; no source or a deleted app is "off".
func (k *Kubernetes) protection(ctx context.Context, app string) (store.Protection, error) {
	src := k.Protection
	if src == nil {
		src, _ = k.env.(ProtectionSource)
	}
	if src == nil {
		return store.Protection{Mode: store.ProtectOff, Scope: store.ScopePreviews}, nil
	}
	p, err := src.ProtectionByAppName(ctx, app)
	if errors.Is(err, store.ErrNotFound) {
		return store.Protection{AppName: app, Mode: store.ProtectOff, Scope: store.ScopePreviews}, nil
	}
	return p, err
}

// routeKind is the protection kind of a route Ingress: its alias kind, ""
// for a deployment URL. ok is false for Ingresses protection ignores.
func routeKind(ing *networkingv1.Ingress) (string, bool) {
	switch ing.Labels[LabelRoute] {
	case RouteDeploy:
		return "", true
	case RouteAlias:
		return ing.Labels[LabelAliasKind], true
	}
	return "", false
}

func (k *Kubernetes) forwardAuthURL() string {
	if k.cfg.ForwardAuthURL != "" {
		return k.cfg.ForwardAuthURL
	}
	return DefaultForwardAuthURL
}

// middlewareObject is the ForwardAuth Middleware of app.
func (k *Kubernetes) middlewareObject(ns, app string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "Middleware",
		"metadata": map[string]any{
			"name": ProtectMiddleware, "namespace": ns,
			"labels": map[string]any{LabelManagedBy: ManagedBy, LabelApp: app},
		},
		"spec": map[string]any{
			"forwardAuth": map[string]any{
				"address": k.forwardAuthURL() + "?app=" + app,
				// X-Forwarded-* always describe the real request.
				"trustForwardHeader": false,
			},
		},
	}}
}

// ensureMiddleware creates or updates the app's Middleware.
func (k *Kubernetes) ensureMiddleware(ctx context.Context, app string) error {
	dc := k.dynamicClient()
	if dc == nil {
		return fmt.Errorf("%w: no dynamic client", ErrTraefikCRD)
	}
	ns := naming.Namespace(app)
	if err := applyObject(ctx, dc.Resource(middlewareGVR).Namespace(ns), k.middlewareObject(ns, app)); err != nil {
		return fmt.Errorf("middleware %s/%s: %w", ns, ProtectMiddleware, err)
	}
	return nil
}

// protectIngress sets or removes want's middleware annotation by the app's
// setting (applyIngress calls it for every route Ingress).
func (k *Kubernetes) protectIngress(ctx context.Context, want *networkingv1.Ingress) error {
	kind, ok := routeKind(want)
	app := want.Labels[LabelApp]
	if !ok || app == "" {
		return nil
	}
	p, err := k.protection(ctx, app)
	if err != nil {
		return fmt.Errorf("protection setting: %w", err)
	}
	if !p.Covers(kind) {
		delete(want.Annotations, AnnotMiddlewares)
		return nil
	}
	if err := k.ensureMiddleware(ctx, app); err != nil {
		return err
	}
	if want.Annotations == nil {
		want.Annotations = map[string]string{}
	}
	want.Annotations[AnnotMiddlewares] = MiddlewareRef(want.Namespace)
	return nil
}

// protectRoute adds the Middleware to a canary IngressRoute (canary.go) of
// a production hostname when the scope covers it.
func (k *Kubernetes) protectRoute(ctx context.Context, app, kind string, ir *unstructured.Unstructured) error {
	p, err := k.protection(ctx, app)
	if err != nil {
		return fmt.Errorf("protection setting: %w", err)
	}
	if !p.Covers(kind) {
		return nil
	}
	if err := k.ensureMiddleware(ctx, app); err != nil {
		return err
	}
	routes, _, _ := unstructured.NestedSlice(ir.Object, "spec", "routes")
	for i := range routes {
		if m, ok := routes[i].(map[string]any); ok {
			m["middlewares"] = []any{map[string]any{"name": ProtectMiddleware}}
		}
	}
	return unstructured.SetNestedSlice(ir.Object, routes, "spec", "routes")
}

// ApplyProtection makes the app's existing route Ingresses and its
// Middleware match the protection setting. It is idempotent
// (routing.Syncer runs it on every sync, after ApplyAliases).
func (k *Kubernetes) ApplyProtection(ctx context.Context, app string) error {
	p, err := k.protection(ctx, app)
	if err != nil {
		return fmt.Errorf("protection setting: %w", err)
	}
	ns := naming.Namespace(app)
	if p.Enabled() {
		if err := k.ensureMiddleware(ctx, app); err != nil {
			return err
		}
	}
	ic := k.client.NetworkingV1().Ingresses(ns)
	list, err := ic.List(ctx, metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedBy})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("list ingresses: %w", err)
	}
	if list != nil {
		for i := range list.Items {
			kind, ok := routeKind(&list.Items[i])
			if !ok {
				continue
			}
			want := ""
			if p.Covers(kind) {
				want = MiddlewareRef(ns)
			}
			if list.Items[i].Annotations[AnnotMiddlewares] == want {
				continue
			}
			name := list.Items[i].Name
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				ing, err := ic.Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if want == "" {
					delete(ing.Annotations, AnnotMiddlewares)
				} else {
					if ing.Annotations == nil {
						ing.Annotations = map[string]string{}
					}
					ing.Annotations[AnnotMiddlewares] = want
				}
				_, err = ic.Update(ctx, ing, metav1.UpdateOptions{})
				return err
			})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("ingress %s/%s: %w", ns, name, err)
			}
		}
	}
	if p.Enabled() {
		return nil
	}
	// Nothing references the Middleware any more (the canary overlay is
	// rewritten by ApplyRollout in the same sync).
	dc := k.dynamicClient()
	if dc == nil {
		return nil
	}
	err = dc.Resource(middlewareGVR).Namespace(ns).Delete(ctx, ProtectMiddleware, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete middleware %s/%s: %w", ns, ProtectMiddleware, err)
	}
	return nil
}
