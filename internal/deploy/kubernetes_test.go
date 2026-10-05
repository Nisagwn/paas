package deploy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nisagwn/paas/internal/store"
)

const (
	sha   = "a3f9c1d2e4b5a6c7d8e9f00112233445566778899"
	image = "localhost:5000/blog:" + sha + "@sha256:abc"
	ns    = "app-blog"
	name  = "d-a3f9c1d"
)

var dep = store.Deployment{ID: 42, AppID: 7, AppName: "blog", CommitSHA: sha[:40], Branch: "feature/Dark-Mode"}

type envMap map[int64]map[string]string

func (e envMap) DeploymentEnv(_ context.Context, d store.Deployment) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range e[d.AppID] {
		out[k] = v
	}
	return out, nil
}

// logs collects log lines; the deployer logs from the test goroutine only,
// but tests read it after a concurrent status updater ran.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) log(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func newDeployer(t *testing.T, env envMap, timeout time.Duration) (*Kubernetes, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	cfg := DefaultConfig()
	cfg.Domain = "paas.test"
	cfg.RunAsNonRoot = true
	cfg.RolloutTimeout = timeout
	cfg.PollInterval = 10 * time.Millisecond
	k, err := New(cs, env, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return k, cs
}

// becomeAvailable plays the Deployment controller: once the Deployment
// exists, it reports all replicas available.
func becomeAvailable(t *testing.T, cs *fake.Clientset) {
	go func() {
		ctx := context.Background()
		for i := 0; i < 500; i++ {
			d, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				d.Status = appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
				if _, err := cs.AppsV1().Deployments(ns).UpdateStatus(ctx, d, metav1.UpdateOptions{}); err == nil {
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Error("deployment never created")
	}()
}

func TestDeployCreatesObjects(t *testing.T) {
	k, cs := newDeployer(t, envMap{7: {"DB_URL": "postgres://x"}}, 5*time.Second)
	becomeAvailable(t, cs)
	var l logs
	if err := k.Deploy(context.Background(), dep, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	ctx := context.Background()

	n, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil || n.Labels[LabelManagedBy] != "paas" {
		t.Fatalf("namespace: %v %v", n.Labels, err)
	}
	q, err := cs.CoreV1().ResourceQuotas(ns).Get(ctx, "paas", metav1.GetOptions{})
	if err != nil || q.Spec.Hard.Pods().Value() != 20 || q.Spec.Hard.Name(corev1.ResourceLimitsMemory, "").String() != "4Gi" {
		t.Fatalf("quota: %v %v", q.Spec.Hard, err)
	}
	if _, err := cs.CoreV1().LimitRanges(ns).Get(ctx, "paas", metav1.GetOptions{}); err != nil {
		t.Fatalf("limit range: %v", err)
	}
	np, err := cs.NetworkingV1().NetworkPolicies(ns).Get(ctx, "paas", metav1.GetOptions{})
	if err != nil || len(np.Spec.Ingress) != 1 ||
		np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
		t.Fatalf("network policy: %+v %v", np, err)
	}

	s, err := cs.CoreV1().Secrets(ns).Get(ctx, name+"-env", metav1.GetOptions{})
	if err != nil || s.Immutable == nil || !*s.Immutable || string(s.Data["DB_URL"]) != "postgres://x" {
		t.Fatalf("secret: %+v %v", s, err)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != name {
		t.Fatalf("secret owner: %+v", s.OwnerReferences)
	}

	d, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"app": "blog", LabelDeploymentID: "42", LabelCommit: sha[:40],
		LabelBranch: "feature-dark-mode", LabelManagedBy: "paas",
	}
	for k, v := range want {
		if d.Labels[k] != v || d.Spec.Template.Labels[k] != v {
			t.Errorf("label %s = %q / %q, want %q", k, d.Labels[k], d.Spec.Template.Labels[k], v)
		}
	}
	if d.Annotations[AnnotBranch] != "feature/Dark-Mode" {
		t.Errorf("branch annotation = %q", d.Annotations[AnnotBranch])
	}
	pod := d.Spec.Template.Spec
	c := pod.Containers[0]
	if c.Image != image {
		t.Errorf("image = %q", c.Image)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["PORT"] != "8080" || env["PAAS_COMMIT_SHA"] != sha[:40] || env["PAAS_APP"] != "blog" ||
		env["PAAS_BRANCH"] != "feature/Dark-Mode" {
		t.Errorf("env = %v", env)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != name+"-env" {
		t.Errorf("envFrom = %+v", c.EnvFrom)
	}
	if c.Resources.Limits.Memory().String() != "256Mi" || c.Resources.Requests.Cpu().String() != "25m" {
		t.Errorf("resources = %+v", c.Resources)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("probe/ports = %+v %+v", c.ReadinessProbe, c.Ports)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("service account token must not be mounted")
	}
	if pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.RunAsUser != nil {
		t.Errorf("pod security context = %+v", pod.SecurityContext)
	}
	if *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("privilege escalation allowed")
	}

	svc, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil || svc.Spec.Type != corev1.ServiceTypeClusterIP || svc.Spec.Ports[0].Port != 80 ||
		svc.Spec.Ports[0].TargetPort.StrVal != "http" || svc.Spec.Selector[LabelDeploymentID] != "42" {
		t.Fatalf("service: %+v %v", svc.Spec, err)
	}
	if !strings.Contains(l.String(), "rollout complete") {
		t.Errorf("log:\n%s", &l)
	}
}

// Re-running the same deployment (worker crashed mid-deploy) must not fail
// on AlreadyExists, and must not touch the running rollout.
func TestDeployIsIdempotent(t *testing.T) {
	env := envMap{7: {"A": "1"}}
	k, cs := newDeployer(t, env, 5*time.Second)
	becomeAvailable(t, cs)
	ctx := context.Background()
	if err := k.Deploy(ctx, dep, image, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	before, _ := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})

	if err := k.Deploy(ctx, dep, image, func(string, ...any) {}); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	after, _ := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if after.ResourceVersion != before.ResourceVersion {
		t.Error("unchanged deployment was updated")
	}

	// Env changed between the attempts: the immutable snapshot is replaced
	// and the pod template rolls.
	env[7]["A"] = "2"
	if err := k.Deploy(ctx, dep, image, func(string, ...any) {}); err != nil {
		t.Fatalf("third deploy: %v", err)
	}
	s, _ := cs.CoreV1().Secrets(ns).Get(ctx, name+"-env", metav1.GetOptions{})
	if string(s.Data["A"]) != "2" {
		t.Errorf("secret not replaced: %q", s.Data["A"])
	}
	after, _ = cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if after.Spec.Template.Annotations[AnnotEnvHash] == before.Spec.Template.Annotations[AnnotEnvHash] {
		t.Error("env hash did not change")
	}

	// A second commit of the same app shares the namespace.
	dep2 := dep
	dep2.ID, dep2.CommitSHA = 43, strings.Repeat("b", 40)
	go func() {
		for {
			d, err := cs.AppsV1().Deployments(ns).Get(ctx, "d-bbbbbbb", metav1.GetOptions{})
			if err == nil {
				d.Status = appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
				cs.AppsV1().Deployments(ns).UpdateStatus(ctx, d, metav1.UpdateOptions{})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if err := k.Deploy(ctx, dep2, image, func(string, ...any) {}); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	list, _ := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if len(list.Items) != 2 {
		t.Errorf("%d deployments, want 2", len(list.Items))
	}
}

func TestDeployTimeout(t *testing.T) {
	k, _ := newDeployer(t, nil, 100*time.Millisecond)
	var l logs
	err := k.Deploy(context.Background(), dep, image, l.log)
	if err == nil || !strings.Contains(err.Error(), "not ready after") || !strings.Contains(err.Error(), "0/1") {
		t.Fatalf("err = %v", err)
	}
}

func addPod(t *testing.T, cs *fake.Clientset, status corev1.PodStatus) {
	t.Helper()
	go func() {
		ctx := context.Background()
		for {
			if _, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-x1", Namespace: ns, Labels: selector(dep)},
			Status:     status,
		}, metav1.CreateOptions{})
	}()
}

func TestDeployCrashLoopFailsFast(t *testing.T) {
	k, cs := newDeployer(t, nil, 10*time.Second)
	addPod(t, cs, corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", RestartCount: 3,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff", Message: "back-off 40s restarting failed container",
			}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
		}},
	})
	var l logs
	start := time.Now()
	err := k.Deploy(context.Background(), dep, image, l.log)
	if err == nil || !strings.Contains(err.Error(), "CrashLoopBackOff") || !strings.Contains(err.Error(), "exit code 1") ||
		!strings.Contains(err.Error(), "fake logs") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("did not fail fast")
	}
	if !strings.Contains(l.String(), "| fake logs") || !strings.Contains(l.String(), "CrashLoopBackOff, 3 restart(s)") {
		t.Errorf("log:\n%s", &l)
	}
}

func TestDeployImagePullFailsFast(t *testing.T) {
	k, cs := newDeployer(t, nil, 10*time.Second)
	addPod(t, cs, corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ImagePullBackOff", Message: `Back-off pulling image "x"`,
			}},
		}},
	})
	err := k.Deploy(context.Background(), dep, image, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployQuotaExceeded(t *testing.T) {
	k, cs := newDeployer(t, nil, 10*time.Second)
	go func() {
		ctx := context.Background()
		for {
			d, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				d.Status.Conditions = []appsv1.DeploymentCondition{{
					Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate",
					Message: `pods "d-a3f9c1d-x" is forbidden: exceeded quota: paas`,
				}}
				cs.AppsV1().Deployments(ns).UpdateStatus(ctx, d, metav1.UpdateOptions{})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	err := k.Deploy(context.Background(), dep, image, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "exceeded quota") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MemoryLimit = "lots"
	if _, err := New(fake.NewClientset(), nil, cfg); err == nil {
		t.Error("invalid quantity accepted")
	}
	cfg = DefaultConfig()
	cfg.CPURequest = "2"
	if _, err := New(fake.NewClientset(), nil, cfg); err == nil {
		t.Error("request above limit accepted")
	}
}

// A named USER cannot be verified by the kubelet; the error says what to do.
func TestDeployNonRootHint(t *testing.T) {
	k, cs := newDeployer(t, nil, 10*time.Second)
	addPod(t, cs, corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason:  "CreateContainerConfigError",
				Message: "container has runAsNonRoot and image has non-numeric user (node), cannot verify user is non-root",
			}},
		}},
	})
	err := k.Deploy(context.Background(), dep, image, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "USER 1000:1000") {
		t.Fatalf("err = %v", err)
	}
}
