package deploy

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

var _ worker.Retirer = (*Kubernetes)(nil)

// Retire deletes the deployment's Kubernetes objects (Faz 7). Deleting the
// Deployment is enough for its Service, Ingress and env Secret: they are
// owned by it and the garbage collector removes them in the background,
// together with the ReplicaSet and pods. The Secret is also deleted
// directly, in case a failed deploy created it but never adopted it.
//
// Objects are only deleted when their deployment-id label matches d, so two
// commits sharing a 7-char prefix never delete each other. Missing objects
// (already retired, or a build that never got to deploy) are not an error.
func (k *Kubernetes) Retire(ctx context.Context, d store.Deployment) error {
	ns, name := naming.Namespace(d.AppName), d.ObjectName()
	id := fmt.Sprint(d.ID)

	dc := k.client.AppsV1().Deployments(ns)
	dep, err := dc.Get(ctx, name, metav1.GetOptions{})
	// ours: no newer deployment of the same commit took over the name.
	ours := apierrors.IsNotFound(err)
	switch {
	case ours:
	case err != nil:
		return fmt.Errorf("deployment %s/%s: %w", ns, name, err)
	case dep.Labels[LabelDeploymentID] == id:
		ours = true
		err := dc.Delete(ctx, name, metav1.DeleteOptions{
			PropagationPolicy: ptr(metav1.DeletePropagationBackground),
			Preconditions:     &metav1.Preconditions{UID: &dep.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete deployment %s/%s: %w", ns, name, err)
		}
	}
	// The per-host certificate of <sha7>-<app> (CertIssuer).
	if ours && k.cfg.CertIssuer != "" {
		tls := certSecretName(name)
		if err := k.client.CoreV1().Secrets(ns).Delete(ctx, tls, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete certificate secret %s/%s: %w", ns, tls, err)
		}
	}

	sc := k.client.CoreV1().Secrets(ns)
	sec, err := sc.Get(ctx, secretName(d), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("secret %s/%s: %w", ns, secretName(d), err)
	case sec.Labels[LabelDeploymentID] == id:
		err := sc.Delete(ctx, sec.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &sec.UID}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete secret %s/%s: %w", ns, sec.Name, err)
		}
	}
	return nil
}
