package deploy

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/nisagwn/paas/internal/naming"
)

// Faz 23: team plans in the cluster.
//
// The namespace quota of an app follows its team's plan: requests.cpu and
// memory are the plan's CPU and Memory instead of PAAS_APP_QUOTA_CPU /
// _MEMORY (add-on reservations stay on top), and the LimitRange defaults
// never exceed the plan's memory. A suspended team's namespaces get a pod
// quota of 0, so nothing can start there (not a woken deployment, not a
// worker scaled by the route reconcile, not a cron run), and SuspendApp
// scales what runs to zero; the objects and volumes are kept. ResumeApp
// brings back the replicas recorded at suspension.

// PlanQuotas is the plan side of the namespace quota (plans.Guard).
type PlanQuotas interface {
	// AppQuota returns the plan's CPU and memory quota of the app's
	// namespace ("" = the configured default) and whether its team is
	// suspended.
	AppQuota(ctx context.Context, app string) (cpu, memory string, suspended bool, err error)
}

// AnnotSuspendedReplicas records the replicas of a workload scaled to
// zero by a suspension; AnnotSuspended marks a CronJob it suspended.
const (
	AnnotSuspendedReplicas = "paas/suspended-replicas"
	AnnotSuspended         = "paas/suspended"
)

// planQuota is the plan's quota of an app's namespace.
type planQuota struct {
	cpu, memory resource.Quantity
	suspended   bool
}

func (k *Kubernetes) planQuota(ctx context.Context, app string) (planQuota, error) {
	pq := planQuota{cpu: k.q.quotaCPU.DeepCopy(), memory: k.q.quotaMem.DeepCopy()}
	if k.Plans == nil {
		return pq, nil
	}
	cpu, mem, suspended, err := k.Plans.AppQuota(ctx, app)
	if err != nil {
		return pq, fmt.Errorf("plan: %w", err)
	}
	pq.suspended = suspended
	if cpu != "" {
		if pq.cpu, err = resource.ParseQuantity(cpu); err != nil {
			return pq, fmt.Errorf("plan cpu %q: %w", cpu, err)
		}
	}
	if mem != "" {
		if pq.memory, err = resource.ParseQuantity(mem); err != nil {
			return pq, fmt.Errorf("plan memory %q: %w", mem, err)
		}
	}
	return pq, nil
}

// applyPlanQuota swaps the configured base of hard (quotaHard) for the
// plan's, keeping the add-on reservations on top, and closes the
// namespace to new pods while the team is suspended.
func (k *Kubernetes) applyPlanQuota(hard corev1.ResourceList, pq planQuota) {
	swap := func(name corev1.ResourceName, base, plan resource.Quantity) {
		v, ok := hard[name]
		if !ok {
			return
		}
		v.Sub(base)
		v.Add(plan)
		hard[name] = v
	}
	swap(corev1.ResourceRequestsCPU, k.q.quotaCPU, pq.cpu)
	swap(corev1.ResourceRequestsMemory, k.q.quotaMem, pq.memory)
	swap(corev1.ResourceLimitsMemory, k.q.quotaMem, pq.memory)
	if pq.suspended {
		hard[corev1.ResourcePods] = *resource.NewQuantity(0, resource.DecimalSI)
	}
}

// limitRangeItem is the container defaults of a namespace: the configured
// requests and limits, the memory capped by the plan's quota.
func (k *Kubernetes) limitRangeItem(pq planQuota) corev1.LimitRangeItem {
	memLim, memReq := k.q.memLim.DeepCopy(), k.q.memReq.DeepCopy()
	if pq.memory.Sign() > 0 && memLim.Cmp(pq.memory) > 0 {
		memLim = pq.memory.DeepCopy()
	}
	if memReq.Cmp(memLim) > 0 {
		memReq = memLim.DeepCopy()
	}
	return corev1.LimitRangeItem{
		Type:           corev1.LimitTypeContainer,
		Default:        corev1.ResourceList{corev1.ResourceCPU: k.q.cpuLim, corev1.ResourceMemory: memLim},
		DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: k.q.cpuReq, corev1.ResourceMemory: memReq},
	}
}

var _ interface {
	SuspendApp(ctx context.Context, app string) error
	ResumeApp(ctx context.Context, app string) error
} = (*Kubernetes)(nil)

// SuspendApp scales every workload of the app to zero and suspends its
// cron jobs. The namespace quota (pods 0) keeps them down; the replicas
// are recorded for ResumeApp. Call it after the team was suspended in the
// database (the quota reads the suspension through Plans).
func (k *Kubernetes) SuspendApp(ctx context.Context, app string) error {
	ns := naming.Namespace(app)
	if err := k.ensureNamespace(ctx, app); err != nil {
		return fmt.Errorf("namespace %s: %w", ns, err)
	}
	sel := metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedBy}
	var errs []error

	deps, err := k.client.AppsV1().Deployments(ns).List(ctx, sel)
	if err != nil {
		return err
	}
	for _, d := range deps.Items {
		if d.Spec.Replicas == nil || *d.Spec.Replicas == 0 {
			continue // asleep (Faz 11) or a stopped worker: stays as it is
		}
		errs = append(errs, k.scaleDeployment(ctx, ns, d.Name, 0, true))
	}
	sets, err := k.client.AppsV1().StatefulSets(ns).List(ctx, sel)
	if err != nil {
		return err
	}
	for _, s := range sets.Items {
		if s.Spec.Replicas == nil || *s.Spec.Replicas == 0 {
			continue
		}
		errs = append(errs, k.scaleStatefulSet(ctx, ns, s.Name, 0, true))
	}
	crons, err := k.client.BatchV1().CronJobs(ns).List(ctx, sel)
	if err != nil {
		return err
	}
	for _, c := range crons.Items {
		if c.Spec.Suspend != nil && *c.Spec.Suspend {
			continue
		}
		errs = append(errs, k.suspendCron(ctx, ns, c.Name, true))
	}
	return errors.Join(errs...)
}

// ResumeApp restores what SuspendApp scaled down. Call it after the
// suspension was lifted in the database (the quota opens again).
func (k *Kubernetes) ResumeApp(ctx context.Context, app string) error {
	ns := naming.Namespace(app)
	if err := k.ensureNamespace(ctx, app); err != nil {
		return fmt.Errorf("namespace %s: %w", ns, err)
	}
	sel := metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedBy}
	var errs []error
	deps, err := k.client.AppsV1().Deployments(ns).List(ctx, sel)
	if err != nil {
		return err
	}
	for _, d := range deps.Items {
		if n, ok := suspendedReplicas(d.Annotations); ok {
			errs = append(errs, k.scaleDeployment(ctx, ns, d.Name, n, false))
		}
	}
	sets, err := k.client.AppsV1().StatefulSets(ns).List(ctx, sel)
	if err != nil {
		return err
	}
	for _, s := range sets.Items {
		if n, ok := suspendedReplicas(s.Annotations); ok {
			errs = append(errs, k.scaleStatefulSet(ctx, ns, s.Name, n, false))
		}
	}
	crons, err := k.client.BatchV1().CronJobs(ns).List(ctx, sel)
	if err != nil {
		return err
	}
	for _, c := range crons.Items {
		if c.Annotations[AnnotSuspended] == "true" {
			errs = append(errs, k.suspendCron(ctx, ns, c.Name, false))
		}
	}
	return errors.Join(errs...)
}

func suspendedReplicas(annot map[string]string) (int32, bool) {
	v, ok := annot[AnnotSuspendedReplicas]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}

// record sets or drops the suspension annotation; it returns the
// annotations to write.
func record(annot map[string]string, old int32, suspend bool) map[string]string {
	if annot == nil {
		annot = map[string]string{}
	}
	if suspend {
		if _, ok := annot[AnnotSuspendedReplicas]; !ok {
			annot[AnnotSuspendedReplicas] = strconv.Itoa(int(old))
		}
	} else {
		delete(annot, AnnotSuspendedReplicas)
	}
	return annot
}

func (k *Kubernetes) scaleDeployment(ctx context.Context, ns, name string, n int32, suspend bool) error {
	dc := k.client.AppsV1().Deployments(ns)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d, err := dc.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		old := int32(1)
		if d.Spec.Replicas != nil {
			old = *d.Spec.Replicas
		}
		d.Annotations = record(d.Annotations, old, suspend)
		d.Spec.Replicas = ptr(n)
		_, err = dc.Update(ctx, d, metav1.UpdateOptions{})
		return err
	})
}

func (k *Kubernetes) scaleStatefulSet(ctx context.Context, ns, name string, n int32, suspend bool) error {
	sc := k.client.AppsV1().StatefulSets(ns)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		s, err := sc.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		old := int32(1)
		if s.Spec.Replicas != nil {
			old = *s.Spec.Replicas
		}
		s.Annotations = record(s.Annotations, old, suspend)
		s.Spec.Replicas = ptr(n)
		_, err = sc.Update(ctx, s, metav1.UpdateOptions{})
		return err
	})
}

func (k *Kubernetes) suspendCron(ctx context.Context, ns, name string, suspend bool) error {
	cc := k.client.BatchV1().CronJobs(ns)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c, err := cc.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		if suspend {
			c.Annotations[AnnotSuspended] = "true"
		} else {
			delete(c.Annotations, AnnotSuspended)
		}
		c.Spec.Suspend = ptr(suspend)
		_, err = cc.Update(ctx, c, metav1.UpdateOptions{})
		return err
	})
}
