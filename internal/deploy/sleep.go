package deploy

// Faz 11: scale to zero. A sleeping deployment keeps all its objects; only
// two things change, and both are undone by Wake:
//
//	Deployment d-<sha7>  replicas 0, annotation paas/sleeping-since
//	Service    d-<sha7>  no selector; EndpointSlice d-<sha7>-activator
//	                     points at the control plane's activator port
//
// Ingresses (deployment route, aliases, extra hosts) are never touched:
// they keep pointing at the Service, so the alias reconcile loop cannot undo
// the sleeping state, and a request for any of the deployment's hostnames
// reaches the activator, which wakes the deployment and serves it.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
)

const (
	// AnnotSleeping is set on a Deployment from Sleep until Wake completes.
	AnnotSleeping = "paas/sleeping-since"
	// activatorManagedBy marks the EndpointSlices paas maintains itself; the
	// EndpointSlice controller ignores slices it does not manage.
	activatorManagedBy = "paas.activator"
	activatorSuffix    = "-activator"
	// wakePoll is how often Wake checks readiness: it bounds cold-start latency.
	wakePoll = 200 * time.Millisecond
)

// ErrNoActivator means Sleep was called without an activator address.
var ErrNoActivator = errors.New("deploy: activator address not configured")

// Workload is the cluster-side state of one deployment.
type Workload struct {
	Namespace, Name string
	DeploymentID    int64
	Replicas        int32 // desired
	Available       int32
	// Sleeping: scaled to zero, or woken but not yet switched back.
	Sleeping bool
	// MetricKey names the Service in Traefik's metrics.
	MetricKey string
}

// Key identifies a workload across namespaces.
func (w Workload) Key() string { return w.Namespace + "/" + w.Name }

// MetricKey is how Traefik's Kubernetes Ingress provider names the backend
// of an Ingress path pointing at Service svc, port "http".
func MetricKey(ns, svc string) string { return ns + "-" + svc + "-http@kubernetes" }

func workloadOf(d *appsv1.Deployment) Workload {
	id, _ := strconv.ParseInt(d.Labels[LabelDeploymentID], 10, 64)
	w := Workload{
		Namespace: d.Namespace, Name: d.Name, DeploymentID: id, Replicas: 1,
		Available: d.Status.AvailableReplicas, MetricKey: MetricKey(d.Namespace, d.Name),
	}
	if d.Spec.Replicas != nil {
		w.Replicas = *d.Spec.Replicas
	}
	_, w.Sleeping = d.Annotations[AnnotSleeping]
	return w
}

// Workloads lists every web Deployment paas manages, in all namespaces.
func (k *Kubernetes) Workloads(ctx context.Context) ([]Workload, error) {
	list, err := k.client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: webWorkloads, // Faz 20: not worker Deployments
	})
	if err != nil {
		return nil, err
	}
	out := make([]Workload, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, workloadOf(&list.Items[i]))
	}
	return out, nil
}

// Sleep scales a deployment to zero. Traffic is moved to the activator
// first, so no request reaches a terminating pod's Service without an
// endpoint. It is idempotent and also repairs a half-done earlier call.
func (k *Kubernetes) Sleep(ctx context.Context, ns, name string) error {
	if k.cfg.ActivatorIP == "" || k.cfg.ActivatorPort == 0 {
		return ErrNoActivator
	}
	dep, err := k.managedDeployment(ctx, ns, name)
	if err != nil {
		return err
	}
	if err := k.pointAtActivator(ctx, dep); err != nil {
		return err
	}
	return k.updateDeployment(ctx, ns, name, func(d *appsv1.Deployment) bool {
		changed := false
		if _, ok := d.Annotations[AnnotSleeping]; !ok {
			if d.Annotations == nil {
				d.Annotations = map[string]string{}
			}
			d.Annotations[AnnotSleeping] = time.Now().UTC().Format(time.RFC3339)
			changed = true
		}
		if d.Spec.Replicas == nil || *d.Spec.Replicas != 0 {
			d.Spec.Replicas = ptr(int32(0))
			changed = true
		}
		return changed
	})
}

// Wake scales a deployment back to one replica, waits until it is
// available (bounded by ctx and RolloutTimeout), and then routes its Service
// to the pods again. It is cheap for an awake deployment.
func (k *Kubernetes) Wake(ctx context.Context, ns, name string) error {
	dep, err := k.managedDeployment(ctx, ns, name)
	if err != nil {
		return err
	}
	if dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 {
		if err := k.updateDeployment(ctx, ns, name, func(d *appsv1.Deployment) bool {
			if d.Spec.Replicas != nil && *d.Spec.Replicas == 0 {
				d.Spec.Replicas = ptr(int32(1))
				return true
			}
			return false
		}); err != nil {
			return err
		}
	}
	if err := k.waitAvailable(ctx, ns, name); err != nil {
		return err
	}
	if err := k.pointAtPods(ctx, dep); err != nil {
		return err
	}
	return k.updateDeployment(ctx, ns, name, func(d *appsv1.Deployment) bool {
		if _, ok := d.Annotations[AnnotSleeping]; !ok {
			return false
		}
		delete(d.Annotations, AnnotSleeping)
		return true
	})
}

// RepointActivator moves the EndpointSlices of all sleeping deployments to
// the current activator address, e.g. after the control plane pod moved.
func (k *Kubernetes) RepointActivator(ctx context.Context) error {
	if k.cfg.ActivatorIP == "" || k.cfg.ActivatorPort == 0 {
		return nil
	}
	list, err := k.client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: webWorkloads,
	})
	if err != nil {
		return err
	}
	var errs []error
	for i := range list.Items {
		d := &list.Items[i]
		if _, ok := d.Annotations[AnnotSleeping]; ok && d.Spec.Replicas != nil && *d.Spec.Replicas == 0 {
			if err := k.pointAtActivator(ctx, d); err != nil {
				errs = append(errs, fmt.Errorf("%s/%s: %w", d.Namespace, d.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Backend finds the workload serving host, the way the ingress controller
// does: through the Ingress rule for host and the Service it points at.
func (k *Kubernetes) Backend(ctx context.Context, host string) (Workload, error) {
	host = strings.ToLower(host)
	list, err := k.client.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedBy,
	})
	if err != nil {
		return Workload{}, err
	}
	for _, ing := range list.Items {
		for _, r := range ing.Spec.Rules {
			if !strings.EqualFold(r.Host, host) || r.HTTP == nil {
				continue
			}
			for _, p := range r.HTTP.Paths {
				if p.Backend.Service == nil {
					continue
				}
				dep, err := k.managedDeployment(ctx, ing.Namespace, p.Backend.Service.Name)
				if err != nil {
					return Workload{}, err
				}
				return workloadOf(dep), nil
			}
		}
	}
	return Workload{}, fmt.Errorf("no route for host %q: %w", host, ErrNotFound)
}

// ErrNotFound is returned for hosts and deployments paas does not serve.
var ErrNotFound = errors.New("not found")

// PodAddr returns host:port of a ready pod of the deployment.
func (k *Kubernetes) PodAddr(ctx context.Context, ns, name string) (string, error) {
	dep, err := k.managedDeployment(ctx, ns, name)
	if err != nil {
		return "", err
	}
	pods, err := k.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String(),
	})
	if err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		if p.Status.PodIP == "" || p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return net.JoinHostPort(p.Status.PodIP, strconv.Itoa(Port)), nil
			}
		}
	}
	return "", fmt.Errorf("%s/%s: no ready pod", ns, name)
}

func (k *Kubernetes) managedDeployment(ctx context.Context, ns, name string) (*appsv1.Deployment, error) {
	dep, err := k.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || (err == nil && dep.Labels[LabelManagedBy] != ManagedBy) {
		return nil, fmt.Errorf("deployment %s/%s: %w", ns, name, ErrNotFound)
	}
	return dep, err
}

func (k *Kubernetes) updateDeployment(ctx context.Context, ns, name string, mutate func(*appsv1.Deployment) bool) error {
	dc := k.client.AppsV1().Deployments(ns)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d, err := dc.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !mutate(d) {
			return nil
		}
		_, err = dc.Update(ctx, d, metav1.UpdateOptions{})
		return err
	})
}

func activatorSliceName(svc string) string { return svc + activatorSuffix }

// pointAtActivator removes the Service's selector (the EndpointSlice
// controller then drops the pod endpoints) and adds an EndpointSlice with
// the activator's address under the Service's port name.
func (k *Kubernetes) pointAtActivator(ctx context.Context, dep *appsv1.Deployment) error {
	want := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: activatorSliceName(dep.Name), Namespace: dep.Namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: dep.Name,
				discoveryv1.LabelManagedBy:   activatorManagedBy,
				LabelManagedBy:               ManagedBy,
				LabelApp:                     dep.Labels[LabelApp],
				LabelDeploymentID:            dep.Labels[LabelDeploymentID],
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef(dep)},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{k.cfg.ActivatorIP},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name: ptr("http"), Port: ptr(k.cfg.ActivatorPort), Protocol: ptr(corev1.ProtocolTCP),
		}},
	}
	if strings.Contains(k.cfg.ActivatorIP, ":") {
		want.AddressType = discoveryv1.AddressTypeIPv6
	}
	if _, err := apply(ctx, k.client.DiscoveryV1().EndpointSlices(dep.Namespace), want.Name, want,
		func(have *discoveryv1.EndpointSlice) bool {
			if have.AddressType == want.AddressType &&
				equality.Semantic.DeepEqual(have.Endpoints, want.Endpoints) &&
				equality.Semantic.DeepEqual(have.Ports, want.Ports) {
				return false
			}
			have.AddressType, have.Endpoints, have.Ports = want.AddressType, want.Endpoints, want.Ports
			return true
		}); err != nil {
		return fmt.Errorf("activator endpoints %s/%s: %w", dep.Namespace, want.Name, err)
	}
	if err := k.setServiceSelector(ctx, dep.Namespace, dep.Name, nil); err != nil {
		return err
	}
	return k.dropPodEndpoints(ctx, dep.Namespace, dep.Name)
}

// dropPodEndpoints removes the pod endpoints left behind once a Service
// loses its selector: the endpoint controllers stop managing, but do not
// delete, the Endpoints object and its EndpointSlice, and the mirroring
// controller copies the stale Endpoints into yet another slice. Left alone,
// the ingress controller would keep sending part of the traffic to the
// address of a terminated pod.
func (k *Kubernetes) dropPodEndpoints(ctx context.Context, ns, svc string) error {
	err := k.client.CoreV1().Endpoints(ns).Delete(ctx, svc, metav1.DeleteOptions{}) //nolint:staticcheck // see above
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete endpoints %s/%s: %w", ns, svc, err)
	}
	ec := k.client.DiscoveryV1().EndpointSlices(ns)
	list, err := ec.List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + svc})
	if err != nil {
		return fmt.Errorf("endpoint slices %s/%s: %w", ns, svc, err)
	}
	for _, es := range list.Items {
		if es.Labels[discoveryv1.LabelManagedBy] == activatorManagedBy {
			continue
		}
		err := ec.Delete(ctx, es.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &es.UID}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete endpoint slice %s/%s: %w", ns, es.Name, err)
		}
	}
	return nil
}

// pointAtPods restores the Service's selector and removes the activator
// EndpointSlice. For a moment both receive traffic; the activator serves
// such requests as well.
func (k *Kubernetes) pointAtPods(ctx context.Context, dep *appsv1.Deployment) error {
	if err := k.setServiceSelector(ctx, dep.Namespace, dep.Name, dep.Spec.Selector.MatchLabels); err != nil {
		return err
	}
	return k.deleteActivatorSlice(ctx, dep.Namespace, dep.Name)
}

func (k *Kubernetes) deleteActivatorSlice(ctx context.Context, ns, svc string) error {
	err := k.client.DiscoveryV1().EndpointSlices(ns).Delete(ctx, activatorSliceName(svc), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete activator endpoints %s/%s: %w", ns, activatorSliceName(svc), err)
	}
	return nil
}

func (k *Kubernetes) setServiceSelector(ctx context.Context, ns, name string, sel map[string]string) error {
	sc := k.client.CoreV1().Services(ns)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		svc, err := sc.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("service %s/%s: %w", ns, name, err)
		}
		if equality.Semantic.DeepEqual(svc.Spec.Selector, sel) || (len(svc.Spec.Selector) == 0 && len(sel) == 0) {
			return nil
		}
		svc.Spec.Selector = sel
		_, err = sc.Update(ctx, svc, metav1.UpdateOptions{})
		return err
	})
}

// waitAvailable polls until the deployment has an available replica. Pod
// states that will not recover (crash loop, missing image) fail at once.
func (k *Kubernetes) waitAvailable(ctx context.Context, ns, name string) error {
	ctx, cancel := context.WithTimeout(ctx, k.cfg.RolloutTimeout)
	defer cancel()
	r := &rollout{k: k, ns: ns, name: name, log: func(string, ...any) {}, seen: map[string]string{}, available: -1}
	tick := time.NewTicker(wakePoll)
	defer tick.Stop()
	for {
		done, err := r.check(ctx)
		if err != nil {
			return fmt.Errorf("wake %s/%s: %w", ns, name, err)
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wake %s/%s: %w (%s)", ns, name, ctx.Err(), r.last)
		case <-tick.C:
		}
	}
}
