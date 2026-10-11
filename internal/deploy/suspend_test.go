package deploy

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakePlans is the plan side of the quota (plans.Guard).
type fakePlans struct {
	cpu, memory string
	suspended   bool
}

func (f *fakePlans) AppQuota(_ context.Context, app string) (string, string, bool, error) {
	return f.cpu, f.memory, f.suspended, nil
}

// The namespace quota and LimitRange follow the team's plan; suspension
// closes the namespace, scales the app to zero and suspends its cron jobs,
// and resuming brings back what ran before.
func TestPlanQuotaAndSuspension(t *testing.T) {
	k, cs := newDeployer(t, nil, 5*time.Second)
	plan := &fakePlans{cpu: "500m", memory: "128Mi"}
	k.Plans = plan
	becomeAvailable(t, cs)
	ctx := context.Background()
	var l logs
	if err := k.Deploy(ctx, dep, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	q, err := cs.CoreV1().ResourceQuotas(ns).Get(ctx, "paas", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hard := q.Spec.Hard
	if hard.Name(corev1.ResourceRequestsCPU, "").String() != "500m" ||
		hard.Name(corev1.ResourceRequestsMemory, "").String() != "128Mi" ||
		hard.Name(corev1.ResourceLimitsMemory, "").String() != "128Mi" || hard.Pods().Value() != 20 {
		t.Fatalf("plan quota: %v", hard)
	}
	lr, err := cs.CoreV1().LimitRanges(ns).Get(ctx, "paas", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	item := lr.Spec.Limits[0]
	if item.Default.Memory().String() != "128Mi" || item.DefaultRequest.Memory().String() != "64Mi" {
		t.Fatalf("limit range capped by the plan: %+v", item)
	}

	// A cron job of the app, running.
	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "c-nightly", Namespace: ns,
		Labels: map[string]string{LabelManagedBy: ManagedBy}}, Spec: batchv1.CronJobSpec{Schedule: "0 3 * * *", Suspend: ptr(false)}}
	if _, err := cs.BatchV1().CronJobs(ns).Create(ctx, cron, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	d, _ := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	d.Spec.Replicas = ptr(int32(2))
	cs.AppsV1().Deployments(ns).Update(ctx, d, metav1.UpdateOptions{})

	plan.suspended = true
	if err := k.SuspendApp(ctx, "blog"); err != nil {
		t.Fatal(err)
	}
	if q, _ := cs.CoreV1().ResourceQuotas(ns).Get(ctx, "paas", metav1.GetOptions{}); q.Spec.Hard.Pods().Value() != 0 {
		t.Fatalf("suspended quota: %v", q.Spec.Hard)
	}
	d, _ = cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if *d.Spec.Replicas != 0 || d.Annotations[AnnotSuspendedReplicas] != "2" {
		t.Fatalf("suspended deployment: %d %v", *d.Spec.Replicas, d.Annotations)
	}
	c, _ := cs.BatchV1().CronJobs(ns).Get(ctx, "c-nightly", metav1.GetOptions{})
	if !*c.Spec.Suspend || c.Annotations[AnnotSuspended] != "true" {
		t.Fatalf("suspended cron: %+v", c.Spec.Suspend)
	}
	// Suspending twice keeps the recorded replicas.
	if err := k.SuspendApp(ctx, "blog"); err != nil {
		t.Fatal(err)
	}

	plan.suspended = false
	if err := k.ResumeApp(ctx, "blog"); err != nil {
		t.Fatal(err)
	}
	if q, _ := cs.CoreV1().ResourceQuotas(ns).Get(ctx, "paas", metav1.GetOptions{}); q.Spec.Hard.Pods().Value() != 20 {
		t.Fatalf("resumed quota: %v", q.Spec.Hard)
	}
	d, _ = cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if *d.Spec.Replicas != 2 || d.Annotations[AnnotSuspendedReplicas] != "" {
		t.Fatalf("resumed deployment: %d %v", *d.Spec.Replicas, d.Annotations)
	}
	c, _ = cs.BatchV1().CronJobs(ns).Get(ctx, "c-nightly", metav1.GetOptions{})
	if *c.Spec.Suspend || c.Annotations[AnnotSuspended] != "" {
		t.Fatalf("resumed cron: %+v %v", c.Spec.Suspend, c.Annotations)
	}

	// Without plans the configured quota applies.
	k.Plans = nil
	if err := k.ensureNamespace(ctx, "blog"); err != nil {
		t.Fatal(err)
	}
	if q, _ := cs.CoreV1().ResourceQuotas(ns).Get(ctx, "paas", metav1.GetOptions{}); q.Spec.Hard.Name(corev1.ResourceRequestsCPU, "").String() != "1" {
		t.Fatalf("default quota: %v", q.Spec.Hard)
	}
}
