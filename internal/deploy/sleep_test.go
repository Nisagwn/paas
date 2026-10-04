package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// sleepyDeployer returns a deployer with an activator address and a
// deployed, available deployment.
func sleepyDeployer(t *testing.T) (*Kubernetes, *fake.Clientset) {
	t.Helper()
	k, cs := newDeployer(t, nil, 5*time.Second)
	k.cfg.ActivatorIP, k.cfg.ActivatorPort = "10.42.0.9", 8081
	becomeAvailable(t, cs)
	var l logs
	if err := k.Deploy(context.Background(), dep, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	return k, cs
}

func getDep(t *testing.T, cs *fake.Clientset) *appsv1.Deployment {
	t.Helper()
	d, err := cs.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func getSvc(t *testing.T, cs *fake.Clientset) *corev1.Service {
	t.Helper()
	s, err := cs.CoreV1().Services(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func assertAsleep(t *testing.T, cs *fake.Clientset, ip string) {
	t.Helper()
	d := getDep(t, cs)
	if *d.Spec.Replicas != 0 || d.Annotations[AnnotSleeping] == "" {
		t.Fatalf("deployment: replicas %d, annotations %v", *d.Spec.Replicas, d.Annotations)
	}
	if sel := getSvc(t, cs).Spec.Selector; len(sel) != 0 {
		t.Fatalf("service selector = %v, want none", sel)
	}
	es, err := cs.DiscoveryV1().EndpointSlices(ns).Get(context.Background(), name+"-activator", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if es.Labels["kubernetes.io/service-name"] != name || es.Labels["endpointslice.kubernetes.io/managed-by"] != activatorManagedBy {
		t.Errorf("slice labels = %v", es.Labels)
	}
	if len(es.Endpoints) != 1 || es.Endpoints[0].Addresses[0] != ip || !*es.Endpoints[0].Conditions.Ready ||
		*es.Ports[0].Name != "http" || *es.Ports[0].Port != 8081 {
		t.Errorf("slice = %+v %+v", es.Endpoints, es.Ports)
	}
	if len(es.OwnerReferences) != 1 || es.OwnerReferences[0].Name != name {
		t.Errorf("slice owner = %+v", es.OwnerReferences)
	}
}

func assertAwake(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	d := getDep(t, cs)
	if *d.Spec.Replicas != 1 {
		t.Fatalf("replicas = %d", *d.Spec.Replicas)
	}
	if _, ok := d.Annotations[AnnotSleeping]; ok {
		t.Fatalf("still annotated: %v", d.Annotations)
	}
	if sel := getSvc(t, cs).Spec.Selector; sel[LabelDeploymentID] != "42" || sel[LabelApp] != "blog" {
		t.Fatalf("service selector = %v", sel)
	}
	_, err := cs.DiscoveryV1().EndpointSlices(ns).Get(context.Background(), name+"-activator", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("activator slice still exists: %v", err)
	}
}

// scaleDown plays the Deployment controller after Sleep.
func scaleDown(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	d := getDep(t, cs)
	d.Status = appsv1.DeploymentStatus{}
	if _, err := cs.AppsV1().Deployments(ns).UpdateStatus(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestSleepAndWake(t *testing.T) {
	k, cs := sleepyDeployer(t)
	ctx := context.Background()

	if err := k.Sleep(ctx, ns, name); err != nil {
		t.Fatal(err)
	}
	assertAsleep(t, cs, "10.42.0.9")
	since := getDep(t, cs).Annotations[AnnotSleeping]
	// Idempotent: a second Sleep changes nothing.
	if err := k.Sleep(ctx, ns, name); err != nil {
		t.Fatal(err)
	}
	if getDep(t, cs).Annotations[AnnotSleeping] != since {
		t.Error("second Sleep reset the time")
	}
	scaleDown(t, cs)

	ws, err := k.Workloads(ctx)
	if err != nil || len(ws) != 1 || !ws[0].Sleeping || ws[0].Replicas != 0 || ws[0].DeploymentID != 42 ||
		ws[0].MetricKey != "app-blog-d-a3f9c1d-http@kubernetes" {
		t.Fatalf("workloads = %+v %v", ws, err)
	}

	// Wake holds until a replica is available, and only then routes the
	// Service back to the pods.
	done := make(chan error, 1)
	go func() { done <- k.Wake(ctx, ns, name) }()
	deadline := time.Now().Add(3 * time.Second)
	for *getDep(t, cs).Spec.Replicas != 1 {
		if time.Now().After(deadline) {
			t.Fatal("Wake did not scale up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(3 * wakePoll)
	select {
	case err := <-done:
		t.Fatalf("Wake returned before the pod was ready: %v", err)
	default:
	}
	if len(getSvc(t, cs).Spec.Selector) != 0 {
		t.Fatal("service switched back before the pod was ready")
	}
	d := getDep(t, cs)
	d.Status = appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	if _, err := cs.AppsV1().Deployments(ns).UpdateStatus(ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertAwake(t, cs)

	// Waking an awake deployment is a no-op.
	if err := k.Wake(ctx, ns, name); err != nil {
		t.Fatal(err)
	}
	assertAwake(t, cs)
}

// TestSleepDropsStalePodEndpoints: once the selector is gone, the endpoint
// controllers leave the old pod endpoints behind; Sleep removes them so no
// request goes to a terminated pod.
func TestSleepDropsStalePodEndpoints(t *testing.T) {
	k, cs := sleepyDeployer(t)
	ctx := context.Background()
	cs.CoreV1().Endpoints(ns).Create(ctx, &corev1.Endpoints{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}, metav1.CreateOptions{})
	for _, mb := range []string{"endpointslice-controller.k8s.io", "endpointslicemirroring-controller.k8s.io"} {
		cs.DiscoveryV1().EndpointSlices(ns).Create(ctx, &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-" + mb[:13], Namespace: ns, Labels: map[string]string{
				discoveryv1.LabelServiceName: name, discoveryv1.LabelManagedBy: mb,
			}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.42.0.17"}}},
		}, metav1.CreateOptions{})
	}
	if err := k.Sleep(ctx, ns, name); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Endpoints(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) { //nolint:staticcheck
		t.Errorf("stale Endpoints kept: %v", err)
	}
	list, _ := cs.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{})
	if len(list.Items) != 1 || list.Items[0].Name != name+"-activator" {
		t.Fatalf("slices = %+v", list.Items)
	}
	assertAsleep(t, cs, "10.42.0.9")
}

func TestSleepNeedsActivator(t *testing.T) {
	k, cs := sleepyDeployer(t)
	k.cfg.ActivatorIP = ""
	if err := k.Sleep(context.Background(), ns, name); !errors.Is(err, ErrNoActivator) {
		t.Fatalf("err = %v", err)
	}
	assertAwake(t, cs)
}

func TestSleepUnknownDeployment(t *testing.T) {
	k, _ := sleepyDeployer(t)
	if err := k.Sleep(context.Background(), ns, "d-0000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestWakeCrashLoopFails(t *testing.T) {
	k, cs := sleepyDeployer(t)
	ctx := context.Background()
	if err := k.Sleep(ctx, ns, name); err != nil {
		t.Fatal(err)
	}
	scaleDown(t, cs)
	cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-x1", Namespace: ns, Labels: selector(dep)},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}, metav1.CreateOptions{})
	err := k.Wake(ctx, ns, name)
	if err == nil || !strings.Contains(err.Error(), "CrashLoopBackOff") {
		t.Fatalf("err = %v", err)
	}
	// Traffic stays with the activator, which answers with an error page.
	if len(getSvc(t, cs).Spec.Selector) != 0 {
		t.Fatal("service points at a crashing pod")
	}
}

func TestRepointActivator(t *testing.T) {
	k, cs := sleepyDeployer(t)
	ctx := context.Background()
	if err := k.Sleep(ctx, ns, name); err != nil {
		t.Fatal(err)
	}
	k.cfg.ActivatorIP = "10.42.0.77" // the control plane restarted elsewhere
	if err := k.RepointActivator(ctx); err != nil {
		t.Fatal(err)
	}
	assertAsleep(t, cs, "10.42.0.77")
}

func TestRedeployWakesSleepingDeployment(t *testing.T) {
	k, cs := sleepyDeployer(t)
	ctx := context.Background()
	if err := k.Sleep(ctx, ns, name); err != nil {
		t.Fatal(err)
	}
	var l logs
	if err := k.Deploy(ctx, dep, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	assertAwake(t, cs)
}

func TestBackendAndPodAddr(t *testing.T) {
	k, cs := sleepyDeployer(t)
	ctx := context.Background()
	w, err := k.Backend(ctx, "A3F9C1D-blog.paas.test")
	if err != nil || w.Key() != ns+"/"+name || w.DeploymentID != 42 {
		t.Fatalf("backend = %+v %v", w, err)
	}
	if _, err := k.Backend(ctx, "nope.paas.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown host: %v", err)
	}

	if _, err := k.PodAddr(ctx, ns, name); err == nil {
		t.Fatal("PodAddr without pods")
	}
	cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-x1", Namespace: ns, Labels: selector(dep)},
		Status: corev1.PodStatus{PodIP: "10.42.0.20", Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}},
	}, metav1.CreateOptions{})
	if addr, err := k.PodAddr(ctx, ns, name); err != nil || addr != "10.42.0.20:8080" {
		t.Fatalf("PodAddr = %q %v", addr, err)
	}
}

func TestNetworkPolicyAllowsActivator(t *testing.T) {
	k, cs := newDeployer(t, nil, 5*time.Second)
	k.cfg.ActivatorNamespace = "paas"
	becomeAvailable(t, cs)
	var l logs
	if err := k.Deploy(context.Background(), dep, image, l.log); err != nil {
		t.Fatal(err)
	}
	np, err := cs.NetworkingV1().NetworkPolicies(ns).Get(context.Background(), "paas", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	from := np.Spec.Ingress[0].From
	if len(from) != 2 || from[1].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "paas" ||
		from[1].PodSelector.MatchLabels["app.kubernetes.io/name"] != "paas" {
		t.Fatalf("peers = %+v", from)
	}
}

func TestParseRequestCounts(t *testing.T) {
	raw := []byte(`# HELP traefik_service_requests_total How many HTTP requests processed on a service.
# TYPE traefik_service_requests_total counter
traefik_service_requests_total{code="200",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 603
traefik_service_requests_total{code="404",method="GET",protocol="http",service="app-blog-d-a3f9c1d-http@kubernetes"} 2
traefik_service_requests_total{code="200",method="GET",protocol="http",service="app-x-d-1111111-http@kubernetes"} 1 1700000000000
traefik_entrypoint_requests_total{code="200",entrypoint="web",method="GET",protocol="http"} 606
`)
	got, err := ParseRequestCounts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got["app-blog-d-a3f9c1d-http@kubernetes"] != 605 || got["app-x-d-1111111-http@kubernetes"] != 1 || len(got) != 2 {
		t.Fatalf("counts = %v", got)
	}

	// No traffic yet: nothing counted, but not an error.
	if got, err := ParseRequestCounts([]byte("go_goroutines 10\n")); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	// Traffic without service labels: idleness is unknowable.
	if _, err := ParseRequestCounts([]byte(`traefik_entrypoint_requests_total{code="200"} 5` + "\n")); err == nil {
		t.Fatal("want error without service labels")
	}
	if _, err := ParseRequestCounts([]byte(`traefik_service_requests_total{service="a"} x` + "\n")); err == nil {
		t.Fatal("want error for a malformed value")
	}
}
