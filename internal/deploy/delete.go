package deploy

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nisagwn/paas/internal/naming"
)

// Faz 28: deleting a project's cluster objects (appdelete.Cluster).
//
// Everything paas creates for an app lives in its namespace app-<name>:
// deployments, services, ingresses, Traefik IngressRoutes, TraefikServices
// and the protection Middleware, process workloads and CronJobs, add-on
// StatefulSets, their PersistentVolumeClaims and backup / copy Jobs, the
// env Secrets, activator EndpointSlices, and the cert-manager Certificates
// and Secrets of custom domains. Deleting the namespace removes all of it.
//
// What outlives the namespace:
//   - PersistentVolumes are cluster-scoped. A volume with the Delete
//     reclaim policy (local-path, most cloud classes) is removed by its
//     provisioner once the claim is gone; one with Retain would keep the
//     database's data forever, so it is deleted here.
//   - Nothing else: the ClusterIssuer of custom domains, Traefik in
//     kube-system and the control plane's namespace hold no per-app
//     objects; the activator's routes live in memory and are dropped with
//     the Services.

// DeleteApp deletes the app's namespaces (app-<name> and any namespace
// paas labelled with the app) and the retained volumes of their claims.
// It reports done once no namespace is left; until then every call issues
// the deletions again, so it is idempotent and survives a restart.
func (k *Kubernetes) DeleteApp(ctx context.Context, app string, appID int64) (bool, error) {
	nsc := k.client.CoreV1().Namespaces()
	names := map[string]bool{naming.Namespace(app): true}
	labelled, err := nsc.List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedBy + "," + LabelApp + "=" + app,
	})
	if err != nil {
		return false, fmt.Errorf("list namespaces: %w", err)
	}
	for _, n := range labelled.Items {
		names[n.Name] = true
	}
	if err := k.deleteRetainedVolumes(ctx, names); err != nil {
		return false, err
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	left := 0
	for _, n := range sorted {
		ns, err := nsc.Get(ctx, n, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("namespace %s: %w", n, err)
		}
		if ns.DeletionTimestamp == nil {
			bg := metav1.DeletePropagationBackground
			err := nsc.Delete(ctx, n, metav1.DeleteOptions{PropagationPolicy: &bg})
			if err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("delete namespace %s: %w", n, err)
			}
		}
		// Terminating until its contents are gone.
		if _, err := nsc.Get(ctx, n, metav1.GetOptions{}); err == nil {
			left++
		} else if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("namespace %s: %w", n, err)
		}
	}
	return left == 0, nil
}

// deleteRetainedVolumes deletes the PersistentVolumes with the Retain
// policy whose claim is in one of namespaces. A bound volume is only
// marked: the pv-protection finalizer holds it until its claim is gone.
func (k *Kubernetes) deleteRetainedVolumes(ctx context.Context, namespaces map[string]bool) error {
	pvc := k.client.CoreV1().PersistentVolumes()
	pvs, err := pvc.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list persistent volumes: %w", err)
	}
	for _, pv := range pvs.Items {
		ref := pv.Spec.ClaimRef
		if ref == nil || !namespaces[ref.Namespace] || pv.DeletionTimestamp != nil ||
			pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
			continue
		}
		if err := pvc.Delete(ctx, pv.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete persistent volume %s: %w", pv.Name, err)
		}
	}
	return nil
}
