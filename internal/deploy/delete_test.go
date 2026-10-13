package deploy

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func pv(name, claimNS string, policy corev1.PersistentVolumeReclaimPolicy) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: policy,
			ClaimRef:                      &corev1.ObjectReference{Namespace: claimNS, Name: "addon-db-data"},
		},
	}
}

func exists(t *testing.T, err error) bool {
	t.Helper()
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// Faz 28: DeleteApp removes the app's namespace (with everything paas put
// in it), other namespaces labelled with the app and retained volumes of
// its claims; it leaves other apps and Delete-policy volumes (their
// provisioner removes them) alone and can run again.
func TestDeleteApp(t *testing.T) {
	k, cs := newDeployer(t, envMap{7: {"KEY": "v"}}, 5*time.Second)
	ctx := context.Background()
	becomeAvailable(t, cs)
	var l logs
	if err := k.Deploy(ctx, dep, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	k.Addons = fakeAddons{pgAddon}
	if _, err := k.ApplyAddon(ctx, pgSpec); err != nil {
		t.Fatal(err)
	}
	// Another app, a second namespace labelled with blog, and volumes.
	if err := k.ensureNamespace(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	extra := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app-blog-jobs",
		Labels: map[string]string{LabelManagedBy: ManagedBy, LabelApp: "blog"}}}
	for _, obj := range []any{extra,
		pv("pv-blog-retain", ns, corev1.PersistentVolumeReclaimRetain),
		pv("pv-blog-jobs", "app-blog-jobs", corev1.PersistentVolumeReclaimRetain),
		pv("pv-blog-delete", ns, corev1.PersistentVolumeReclaimDelete),
		pv("pv-shop-retain", "app-shop", corev1.PersistentVolumeReclaimRetain),
	} {
		var err error
		switch o := obj.(type) {
		case *corev1.Namespace:
			_, err = cs.CoreV1().Namespaces().Create(ctx, o, metav1.CreateOptions{})
		case *corev1.PersistentVolume:
			_, err = cs.CoreV1().PersistentVolumes().Create(ctx, o, metav1.CreateOptions{})
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	done, err := k.DeleteApp(ctx, "blog", 7)
	if err != nil || !done {
		t.Fatalf("DeleteApp: done %v, %v", done, err)
	}
	for _, n := range []string{ns, "app-blog-jobs"} {
		if _, err := cs.CoreV1().Namespaces().Get(ctx, n, metav1.GetOptions{}); exists(t, err) {
			t.Errorf("namespace %s survived", n)
		}
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, "app-shop", metav1.GetOptions{}); !exists(t, err) {
		t.Error("another app's namespace was deleted")
	}
	for name, want := range map[string]bool{"pv-blog-retain": false, "pv-blog-jobs": false, "pv-blog-delete": true, "pv-shop-retain": true} {
		_, err := cs.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
		if got := exists(t, err); got != want {
			t.Errorf("volume %s exists = %v, want %v", name, got, want)
		}
	}

	// Idempotent: nothing left, nothing to do.
	if done, err := k.DeleteApp(ctx, "blog", 7); err != nil || !done {
		t.Fatalf("again: done %v, %v", done, err)
	}
	// An app that never deployed has nothing in the cluster.
	if done, err := k.DeleteApp(ctx, "never", 8); err != nil || !done {
		t.Fatalf("never deployed: done %v, %v", done, err)
	}
}

// A terminating namespace is not deleted again, and the deletion is not
// done until it is gone.
func TestDeleteAppWaitsForTermination(t *testing.T) {
	cs := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: ns, DeletionTimestamp: &metav1.Time{Time: time.Now()}, Finalizers: []string{"kubernetes"},
		Labels: map[string]string{LabelManagedBy: ManagedBy, LabelApp: "blog"},
	}})
	cfg := DefaultConfig()
	cfg.Domain = "paas.test"
	k, err := New(cs, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if done, err := k.DeleteApp(ctx, "blog", 7); err != nil || done {
		t.Fatalf("terminating: done %v, %v", done, err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "namespaces" {
			t.Fatal("a terminating namespace was deleted again")
		}
	}
	if err := cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("namespaces"), "", ns); err != nil {
		t.Fatal(err)
	}
	if done, err := k.DeleteApp(ctx, "blog", 7); err != nil || !done {
		t.Fatalf("after termination: done %v, %v", done, err)
	}
}
