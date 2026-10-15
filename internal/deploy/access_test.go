package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func accessDeployer(t *testing.T) (*Kubernetes, *fake.Clientset) {
	t.Helper()
	k, cs := newDeployer(t, nil, time.Second)
	k.cfg.ServiceAccountNamespace, k.cfg.ServiceAccountName = "paas", "paas"
	return k, cs
}

func countActions(cs *fake.Clientset, verb, resource string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

func assertBinding(t *testing.T, cs *fake.Clientset, ns string) {
	t.Helper()
	rb, err := cs.RbacV1().RoleBindings(ns).Get(context.Background(), AppAccessBinding, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "paas-app-manager"}
	if rb.RoleRef != want {
		t.Errorf("roleRef = %+v", rb.RoleRef)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0] != (rbacv1.Subject{Kind: "ServiceAccount", Namespace: "paas", Name: "paas"}) {
		t.Errorf("subjects = %+v", rb.Subjects)
	}
	if rb.Labels[LabelManagedBy] != ManagedBy {
		t.Errorf("labels = %v", rb.Labels)
	}
}

func TestEnsureAppAccessIdempotent(t *testing.T) {
	k, cs := accessDeployer(t)
	ctx := context.Background()
	if err := k.EnsureAppAccess(ctx, "app-blog"); err != nil {
		t.Fatal(err)
	}
	assertBinding(t, cs, "app-blog")
	if n := countActions(cs, "list", "resourcequotas"); n != 1 {
		t.Errorf("waited with %d probes, want 1", n)
	}

	cs.ClearActions()
	if err := k.EnsureAppAccess(ctx, "app-blog"); err != nil {
		t.Fatal(err)
	}
	if n := countActions(cs, "update", "rolebindings"); n != 0 {
		t.Errorf("unchanged binding updated %d times", n)
	}
	if n := countActions(cs, "list", "resourcequotas"); n != 0 {
		t.Errorf("existing binding waited for")
	}
}

func TestEnsureAppAccessRepairsSubject(t *testing.T) {
	k, cs := accessDeployer(t)
	ctx := context.Background()
	stale := k.accessBinding("app-blog")
	stale.Labels = nil
	stale.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "old", Name: "paas"}}
	if _, err := cs.RbacV1().RoleBindings("app-blog").Create(ctx, stale, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := k.EnsureAppAccess(ctx, "app-blog"); err != nil {
		t.Fatal(err)
	}
	assertBinding(t, cs, "app-blog")

	// roleRef is immutable: a binding of another role is reported.
	other := k.accessBinding("app-shop")
	other.RoleRef.Name = "admin"
	if _, err := cs.RbacV1().RoleBindings("app-shop").Create(ctx, other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := k.EnsureAppAccess(ctx, "app-shop"); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureAppAccessScope(t *testing.T) {
	k, cs := accessDeployer(t)
	ctx := context.Background()
	for _, ns := range []string{"kube-system", "paas", "default"} {
		if err := k.EnsureAppAccess(ctx, ns); err == nil {
			t.Errorf("%s: no error", ns)
		}
	}
	if n := countActions(cs, "create", "rolebindings"); n != 0 {
		t.Errorf("%d rolebindings created outside app namespaces", n)
	}

	// Without a ServiceAccount (kubeconfig user) nothing is bound.
	k.cfg.ServiceAccountName = ""
	if err := k.EnsureAppAccess(ctx, "app-blog"); err != nil {
		t.Fatal(err)
	}
	if names, err := k.EnsureAllAppAccess(ctx); err != nil || names != nil {
		t.Fatalf("EnsureAllAppAccess = %v, %v", names, err)
	}
	if n := countActions(cs, "create", "rolebindings"); n != 0 {
		t.Errorf("%d rolebindings created without a service account", n)
	}
}

func TestEnsureAppAccessWaitsForAuthorizer(t *testing.T) {
	k, cs := accessDeployer(t)
	probes := 0
	cs.PrependReactor("list", "resourcequotas", func(k8stesting.Action) (bool, runtime.Object, error) {
		probes++
		if probes < 3 {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "resourcequotas"}, "", nil)
		}
		return false, nil, nil
	})
	if err := k.EnsureAppAccess(context.Background(), "app-blog"); err != nil {
		t.Fatal(err)
	}
	if probes != 3 {
		t.Errorf("probes = %d, want 3", probes)
	}

	// Forbidden for good: an error naming the cause.
	old := accessReadyTimeout
	accessReadyTimeout = 300 * time.Millisecond
	defer func() { accessReadyTimeout = old }()
	cs.PrependReactor("list", "resourcequotas", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "resourcequotas"}, "", nil)
	})
	err := k.EnsureAppAccess(context.Background(), "app-shop")
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployBindsAppManager(t *testing.T) {
	k, cs := accessDeployer(t)
	becomeAvailable(t, cs)
	var l logs
	if err := k.Deploy(context.Background(), dep, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	assertBinding(t, cs, ns)

	// The binding comes right after the namespace, before anything in it.
	var order []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			order = append(order, a.GetResource().Resource)
		}
	}
	if len(order) < 3 || order[0] != "namespaces" || order[1] != "rolebindings" {
		t.Errorf("create order = %v", order)
	}
}

func TestEnsureAllAppAccess(t *testing.T) {
	k, cs := accessDeployer(t)
	ctx := context.Background()
	managed := map[string]string{LabelManagedBy: ManagedBy}
	for name, lbl := range map[string]map[string]string{
		"app-blog": managed, "app-shop": managed,
		"app-foreign": nil, // not created by paas
		"kube-system": managed,
	} {
		if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbl},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	names, err := k.EnsureAllAppAccess(ctx)
	if err != nil || strings.Join(names, ",") != "app-blog,app-shop" {
		t.Fatalf("names = %v, %v", names, err)
	}
	assertBinding(t, cs, "app-blog")
	assertBinding(t, cs, "app-shop")
	for _, ns := range []string{"app-foreign", "kube-system"} {
		if _, err := cs.RbacV1().RoleBindings(ns).Get(ctx, AppAccessBinding, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s: binding %v", ns, err)
		}
	}
	// A second run changes nothing.
	cs.ClearActions()
	if _, err := k.EnsureAllAppAccess(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countActions(cs, "update", "rolebindings") + countActions(cs, "list", "resourcequotas"); n != 0 {
		t.Errorf("second run: %v", cs.Actions())
	}
}

func TestDetectServiceAccount(t *testing.T) {
	for _, tc := range []struct{ user, want string }{
		{"system:serviceaccount:paas:paas", "paas/paas"},
		{"system:admin", ""},
		{"system:serviceaccount:broken", ""},
	} {
		k, cs := newDeployer(t, nil, time.Second)
		cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, &authnv1.SelfSubjectReview{Status: authnv1.SelfSubjectReviewStatus{
				UserInfo: authnv1.UserInfo{Username: tc.user},
			}}, nil
		})
		user, err := k.DetectServiceAccount(context.Background())
		if err != nil || user != tc.user || k.ServiceAccount() != tc.want {
			t.Errorf("%s: user %q, service account %q, err %v", tc.user, user, k.ServiceAccount(), err)
		}
	}
}

func TestNamespacePodSecurity(t *testing.T) {
	k, cs := accessDeployer(t)
	ctx := context.Background()
	// An operator tightened app-shop; app-blog has no level yet.
	for name, level := range map[string]string{"app-shop": "restricted", "app-blog": ""} {
		lbl := map[string]string{}
		if level != "" {
			lbl[LabelPodSecurity] = level
		}
		if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbl},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for app, want := range map[string]string{"shop": "restricted", "blog": "baseline", "new": "baseline"} {
		if err := k.ensureNamespace(ctx, app); err != nil {
			t.Fatal(err)
		}
		n, err := cs.CoreV1().Namespaces().Get(ctx, "app-"+app, metav1.GetOptions{})
		if err != nil || n.Labels[LabelPodSecurity] != want || n.Labels[LabelManagedBy] != ManagedBy {
			t.Errorf("%s: labels %v, %v", app, n.Labels, err)
		}
	}
}
