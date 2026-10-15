package deploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/nisagwn/paas/internal/naming"
)

// Faz 29: least-privilege RBAC.
//
// The control plane's ServiceAccount holds no write access to namespaced
// objects cluster-wide (infra/k8s/30-control-plane-rbac.yaml). Its per-app
// verbs live in the ClusterRole paas-app-manager, which is bound only
// inside the app namespaces: when paas creates app-<name> it also creates
// the RoleBinding AppAccessBinding there. RBAC lets it bind that one role
// (the "bind" verb with resourceNames), and a ValidatingAdmissionPolicy
// rejects any RoleBinding, namespace change or volume deletion of the
// ServiceAccount outside the app namespaces, so a compromised control plane
// cannot grant itself the role in kube-system or its own namespace.

const (
	// AppManagerRole is the ClusterRole with every verb paas needs inside
	// an app namespace.
	AppManagerRole = "paas-app-manager"
	// AppAccessBinding names the RoleBinding of AppManagerRole in each app
	// namespace.
	AppAccessBinding = "paas-control-plane"

	// LabelPodSecurity / PodSecurityLevel: the Pod Security Admission level
	// enforced in app namespaces; the admission policy refuses an app
	// namespace of the control plane without it.
	LabelPodSecurity = "pod-security.kubernetes.io/enforce"
	PodSecurityLevel = "baseline"
)

// accessReadyTimeout bounds the wait for a new RoleBinding to reach the
// API server's authorizer (its informer cache; usually milliseconds).
var accessReadyTimeout = 10 * time.Second

// ServiceAccountUser splits a Kubernetes user name of the form
// system:serviceaccount:<namespace>:<name>; ok is false for any other user.
func ServiceAccountUser(user string) (namespace, name string, ok bool) {
	rest, found := strings.CutPrefix(user, "system:serviceaccount:")
	if !found {
		return "", "", false
	}
	namespace, name, found = strings.Cut(rest, ":")
	if !found || namespace == "" || name == "" || strings.Contains(name, ":") {
		return "", "", false
	}
	return namespace, name, true
}

// DetectServiceAccount asks the API server who paas is (SelfSubjectReview,
// allowed to every authenticated user) and, when that is a ServiceAccount,
// makes it the subject of the app namespaces' RoleBindings. A configured
// ServiceAccount is kept. With any other identity (a kubeconfig user on a
// laptop cluster) EnsureAppAccess does nothing. It returns the user name.
func (k *Kubernetes) DetectServiceAccount(ctx context.Context) (string, error) {
	r, err := k.client.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("self subject review: %w", err)
	}
	user := r.Status.UserInfo.Username
	if k.cfg.ServiceAccountName == "" {
		if ns, name, ok := ServiceAccountUser(user); ok {
			k.cfg.ServiceAccountNamespace, k.cfg.ServiceAccountName = ns, name
		}
	}
	return user, nil
}

// ServiceAccount returns the subject of the app namespaces' RoleBindings
// ("" when paas runs under another identity).
func (k *Kubernetes) ServiceAccount() string {
	if k.cfg.ServiceAccountName == "" {
		return ""
	}
	return k.cfg.ServiceAccountNamespace + "/" + k.cfg.ServiceAccountName
}

func (k *Kubernetes) accessBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: AppAccessBinding, Namespace: ns,
			Labels: map[string]string{LabelManagedBy: ManagedBy},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: AppManagerRole},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Namespace: k.cfg.ServiceAccountNamespace, Name: k.cfg.ServiceAccountName,
		}},
	}
}

// EnsureAppAccess binds AppManagerRole to the control plane's ServiceAccount
// in the app namespace ns, which must exist. It is idempotent: an existing
// binding with the right role and subject is left alone, a wrong subject
// is corrected. A new binding is waited for until the API server
// authorizes requests through it. Without a ServiceAccount it does nothing.
func (k *Kubernetes) EnsureAppAccess(ctx context.Context, ns string) error {
	if k.cfg.ServiceAccountName == "" {
		return nil
	}
	if !strings.HasPrefix(ns, naming.Namespace("")) {
		return fmt.Errorf("access: %q is not an app namespace", ns)
	}
	want := k.accessBinding(ns)
	rbc := k.client.RbacV1().RoleBindings(ns)
	// Create first: before the binding exists paas may not read
	// RoleBindings in the namespace (paas-app-manager grants that).
	_, err := rbc.Create(ctx, want, metav1.CreateOptions{})
	if err == nil {
		return k.waitAccess(ctx, ns)
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("access %s: %w", ns, err)
	}
	have, err := rbc.Get(ctx, AppAccessBinding, metav1.GetOptions{})
	if apierrors.IsForbidden(err) {
		// A concurrent call created it a moment ago; the authorizer has
		// not seen it yet.
		if err := k.waitAccess(ctx, ns); err != nil {
			return err
		}
		have, err = rbc.Get(ctx, AppAccessBinding, metav1.GetOptions{})
	}
	if err != nil {
		return fmt.Errorf("access %s: %w", ns, err)
	}
	if have.RoleRef != want.RoleRef {
		// roleRef is immutable and paas may not delete RoleBindings.
		return fmt.Errorf("access %s: rolebinding %s refers to %s %s, want ClusterRole %s",
			ns, AppAccessBinding, have.RoleRef.Kind, have.RoleRef.Name, AppManagerRole)
	}
	relabelled := mergeLabels(&have.Labels, want.Labels)
	if slices.Equal(have.Subjects, want.Subjects) && !relabelled {
		return nil
	}
	have.Subjects = want.Subjects
	if _, err := rbc.Update(ctx, have, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("access %s: %w", ns, err)
	}
	return nil
}

// waitAccess polls a request paas-app-manager allows until the authorizer
// stops answering Forbidden.
func (k *Kubernetes) waitAccess(ctx context.Context, ns string) error {
	ctx, cancel := context.WithTimeout(ctx, accessReadyTimeout)
	defer cancel()
	var last error
	err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		_, last = k.client.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{Limit: 1})
		if apierrors.IsForbidden(last) {
			return false, nil
		}
		return true, last
	})
	if err != nil && last != nil {
		err = last // Forbidden until the deadline, or another error
	}
	if err != nil {
		return fmt.Errorf("access %s: waiting for rolebinding: %w", ns, err)
	}
	return nil
}

// AppNamespaces lists the namespaces paas created: app-<name> with the
// managed-by label. Listing namespaces is one of the few cluster-wide
// reads the control plane keeps.
func (k *Kubernetes) AppNamespaces(ctx context.Context) ([]string, error) {
	list, err := k.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedBy,
	})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	var out []string
	for _, n := range list.Items {
		if strings.HasPrefix(n.Name, naming.Namespace("")) && n.DeletionTimestamp == nil {
			out = append(out, n.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// EnsureAllAppAccess runs EnsureAppAccess for every app namespace, so
// namespaces created before Faz 29 get their RoleBinding at startup. It
// returns the namespaces it covered; errors of single namespaces are
// joined and do not stop the others.
func (k *Kubernetes) EnsureAllAppAccess(ctx context.Context) ([]string, error) {
	if k.cfg.ServiceAccountName == "" {
		return nil, nil
	}
	names, err := k.AppNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, ns := range names {
		if err := k.EnsureAppAccess(ctx, ns); err != nil {
			errs = append(errs, err)
		}
	}
	return names, errors.Join(errs...)
}
