package deploy

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
)

// fakeProcesses is a ProcessSource.
type fakeProcesses struct {
	sets      map[int64]process.Set
	overrides map[string]int
}

func (f fakeProcesses) DeploymentProcesses(_ context.Context, id int64) (process.Set, error) {
	return f.sets[id], nil
}

func (f fakeProcesses) ProcessReplicas(context.Context, int64) (map[string]int, error) {
	return f.overrides, nil
}

var procSet = process.Set{
	Source: "paas.yaml",
	Workers: []process.Worker{
		{Name: "queue", Command: "node queue.js", Exec: []string{"sh", "-c", "node queue.js"}, Replicas: 2, Previews: true},
		{Name: "bot", Command: "node bot.js", Exec: []string{"sh", "-c", "node bot.js"}, Replicas: 1},
	},
	Crons: []process.Cron{{Name: "cleanup", Schedule: "*/15 * * * *", Command: "node c.js", Exec: []string{"sh", "-c", "node c.js"}}},
}

// available plays the Deployment controller for the named Deployments:
// once one exists, it reports its desired replicas available.
func available(t *testing.T, cs *fake.Clientset, names ...string) {
	t.Helper()
	for _, n := range names {
		go func(n string) {
			ctx := context.Background()
			for i := 0; i < 1000; i++ {
				d, err := cs.AppsV1().Deployments(ns).Get(ctx, n, metav1.GetOptions{})
				if err == nil {
					r := *d.Spec.Replicas
					d.Status = appsv1.DeploymentStatus{Replicas: r, UpdatedReplicas: r, ReadyReplicas: r, AvailableReplicas: r}
					if _, err := cs.AppsV1().Deployments(ns).UpdateStatus(ctx, d, metav1.UpdateOptions{}); err == nil {
						return
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
		}(n)
	}
}

func envOf(c corev1.Container) map[string]string {
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	return env
}

func TestDeployStartsProcesses(t *testing.T) {
	k, cs := newDeployer(t, envMap{7: {"QUEUE_URL": "x"}}, 5*time.Second)
	k.Processes = fakeProcesses{sets: map[int64]process.Set{42: procSet}, overrides: map[string]int{"queue": 3}}
	prod := dep
	prod.Target = store.EnvProduction
	available(t, cs, name, name+"-w-queue", name+"-w-bot")
	var l logs
	ctx := context.Background()
	if err := k.Deploy(ctx, prod, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}

	web, _ := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if env := envOf(web.Spec.Template.Spec.Containers[0]); env["PAAS_PROCESS"] != "web" {
		t.Errorf("web env = %v", env)
	}
	if web.Spec.Template.Spec.Containers[0].Command != nil {
		t.Errorf("web command overridden: %q", web.Spec.Template.Spec.Containers[0].Command)
	}

	w, err := cs.AppsV1().Deployments(ns).Get(ctx, name+"-w-queue", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *w.Spec.Replicas != 3 { // the app's override wins over paas.yaml
		t.Errorf("queue replicas = %d, want 3", *w.Spec.Replicas)
	}
	if w.Spec.MinReadySeconds != workerMinReady {
		t.Errorf("minReadySeconds = %d", w.Spec.MinReadySeconds)
	}
	if len(w.OwnerReferences) != 1 || w.OwnerReferences[0].Kind != "Deployment" || w.OwnerReferences[0].Name != name {
		t.Errorf("owner = %+v", w.OwnerReferences)
	}
	wantSel := map[string]string{LabelApp: "blog", LabelProcessOf: "42", LabelProcess: "queue"}
	if !reflect.DeepEqual(w.Spec.Selector.MatchLabels, wantSel) {
		t.Errorf("selector = %v", w.Spec.Selector.MatchLabels)
	}
	pl := w.Spec.Template.Labels
	if _, ok := pl[LabelDeploymentID]; ok || pl[LabelProcessKind] != "worker" || pl[LabelCommit] != sha[:40] {
		// The web Service selects app + deployment-id: worker pods must not match it.
		t.Errorf("worker pod labels = %v", pl)
	}
	c := w.Spec.Template.Spec.Containers[0]
	if !reflect.DeepEqual(c.Command, []string{"sh", "-c", "node queue.js"}) || c.Image != image ||
		len(c.Ports) != 0 || c.ReadinessProbe != nil || c.EnvFrom[0].SecretRef.Name != name+"-env" ||
		*c.SecurityContext.AllowPrivilegeEscalation || c.Resources.Limits.Memory().String() != "256Mi" {
		t.Errorf("worker container = %+v", c)
	}
	if env := envOf(c); env["PAAS_PROCESS"] != "queue" || env["PORT"] != "8080" || env["PAAS_APP"] != "blog" {
		t.Errorf("worker env = %v", env)
	}
	if w, _ := cs.AppsV1().Deployments(ns).Get(ctx, name+"-w-bot", metav1.GetOptions{}); *w.Spec.Replicas != 1 {
		t.Errorf("bot replicas = %d", *w.Spec.Replicas)
	}

	cj, err := cs.BatchV1().CronJobs(ns).Get(ctx, name+"-c-cleanup", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !*cj.Spec.Suspend || cj.Spec.Schedule != "*/15 * * * *" || cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent ||
		*cj.Spec.StartingDeadlineSeconds != cronStartDeadline || *cj.Spec.SuccessfulJobsHistoryLimit != cronHistory {
		t.Errorf("cron spec = %+v", cj.Spec)
	}
	js := cj.Spec.JobTemplate.Spec
	if *js.ActiveDeadlineSeconds != cronRunDeadline || js.Template.Spec.RestartPolicy != corev1.RestartPolicyNever ||
		envOf(js.Template.Spec.Containers[0])["PAAS_PROCESS"] != "cleanup" || js.Template.Labels[LabelProcess] != "cleanup" {
		t.Errorf("job template = %+v", js)
	}
	if cj.OwnerReferences[0].Name != name {
		t.Errorf("cron owner = %+v", cj.OwnerReferences)
	}
	for _, want := range []string{"worker queue: 3 replica(s)", "cron cleanup (*/15 * * * *, UTC): scheduled once",
		"waiting for worker queue", "waiting for worker bot"} {
		if !strings.Contains(l.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, &l)
		}
	}

	// A retry of the same deployment changes nothing.
	before, _ := cs.AppsV1().Deployments(ns).Get(ctx, name+"-w-queue", metav1.GetOptions{})
	if err := k.Deploy(ctx, prod, image, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	after, _ := cs.AppsV1().Deployments(ns).Get(ctx, name+"-w-queue", metav1.GetOptions{})
	if after.ResourceVersion != before.ResourceVersion {
		t.Error("unchanged worker was updated")
	}

	// Scale to zero never sees workers.
	wls, err := k.Workloads(ctx)
	if err != nil || len(wls) != 1 || wls[0].Name != name {
		t.Errorf("workloads = %+v, %v", wls, err)
	}
}

func TestDeployPreviewRunsPreviewProcessesOnly(t *testing.T) {
	k, cs := newDeployer(t, nil, 5*time.Second)
	k.Processes = fakeProcesses{sets: map[int64]process.Set{42: procSet}, overrides: map[string]int{"queue": 5}}
	available(t, cs, name, name+"-w-queue")
	var l logs
	ctx := context.Background()
	if err := k.Deploy(ctx, dep, image, l.log); err != nil { // dep.Target is preview
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	q, _ := cs.AppsV1().Deployments(ns).Get(ctx, name+"-w-queue", metav1.GetOptions{})
	b, _ := cs.AppsV1().Deployments(ns).Get(ctx, name+"-w-bot", metav1.GetOptions{})
	if *q.Spec.Replicas != 2 || *b.Spec.Replicas != 0 {
		t.Errorf("replicas queue=%d bot=%d, want 2 (paas.yaml, no override) and 0", *q.Spec.Replicas, *b.Spec.Replicas)
	}
	if !strings.Contains(l.String(), "worker bot: not run in previews") ||
		!strings.Contains(l.String(), "not scheduled in previews") || strings.Contains(l.String(), "waiting for worker bot") {
		t.Errorf("log:\n%s", &l)
	}
}

func TestDeployCrashingWorkerFails(t *testing.T) {
	k, cs := newDeployer(t, nil, 10*time.Second)
	k.Processes = fakeProcesses{sets: map[int64]process.Set{42: {Workers: procSet.Workers[1:]}}}
	prod := dep
	prod.Target = store.EnvProduction
	available(t, cs, name)
	go func() {
		ctx := context.Background()
		for {
			if _, err := cs.AppsV1().Deployments(ns).Get(ctx, name+"-w-bot", metav1.GetOptions{}); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-w-bot-x1", Namespace: ns, Labels: processSelector(prod, "bot")},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", RestartCount: 2,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}}},
		}, metav1.CreateOptions{})
	}()
	start := time.Now()
	err := k.Deploy(context.Background(), prod, image, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "worker bot") || !strings.Contains(err.Error(), "CrashLoopBackOff") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("did not fail fast")
	}
}

func TestDeployWithoutWeb(t *testing.T) {
	k, cs := newDeployer(t, nil, 5*time.Second)
	set := process.Set{NoWeb: true, Workers: procSet.Workers[1:]}
	k.Processes = fakeProcesses{sets: map[int64]process.Set{42: set}}
	prod := dep
	prod.Target = store.EnvProduction
	available(t, cs, name+"-w-bot")
	var l logs
	ctx := context.Background()
	if err := k.Deploy(ctx, prod, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	if _, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Error("web Deployment created")
	}
	if svcs, _ := cs.CoreV1().Services(ns).List(ctx, metav1.ListOptions{}); len(svcs.Items) != 0 {
		t.Errorf("services: %d", len(svcs.Items))
	}
	if ings, _ := cs.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{}); len(ings.Items) != 0 {
		t.Errorf("ingresses: %d", len(ings.Items))
	}
	w, err := cs.AppsV1().Deployments(ns).Get(ctx, name+"-w-bot", metav1.GetOptions{})
	if err != nil || w.OwnerReferences[0].Kind != "Secret" || w.OwnerReferences[0].Name != name+"-env" {
		t.Fatalf("worker owner = %+v, %v", w, err)
	}
	if !strings.Contains(l.String(), "no web process") {
		t.Errorf("log:\n%s", &l)
	}
	// Scale to zero has nothing to do with it.
	if wls, _ := k.Workloads(ctx); len(wls) != 0 {
		t.Errorf("workloads = %+v", wls)
	}
	// Retire deletes the env Secret; the garbage collector takes the workers.
	if err := k.Retire(ctx, prod); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Secrets(ns).Get(ctx, name+"-env", metav1.GetOptions{}); err == nil {
		t.Error("secret not deleted")
	}
}

func TestDeployWebCommandOverride(t *testing.T) {
	k, cs := newDeployer(t, nil, 5*time.Second)
	k.Processes = fakeProcesses{sets: map[int64]process.Set{42: {WebCommand: "./serve", WebExec: []string{"sh", "-c", "./serve"}}}}
	available(t, cs, name)
	ctx := context.Background()
	if err := k.Deploy(ctx, dep, image, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	web, _ := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if got := web.Spec.Template.Spec.Containers[0].Command; !reflect.DeepEqual(got, []string{"sh", "-c", "./serve"}) {
		t.Errorf("web command = %q", got)
	}
}

// One generation per app: the reconcile scales workers and suspends crons
// of every deployment the plan does not run, and leaves in-flight ones alone.
func TestApplyProcesses(t *testing.T) {
	k, cs := newDeployer(t, nil, 5*time.Second)
	ctx := context.Background()
	owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "x", UID: "u"}
	deps := []store.Deployment{dep, dep, dep}
	for i := range deps {
		deps[i].ID = int64(41 + i)
		deps[i].CommitSHA = strings.Repeat(string(rune('a'+i)), 40)
		if err := k.ensureNamespace(ctx, "blog"); err != nil {
			t.Fatal(err)
		}
		if err := k.startProcesses(ctx, deps[i], image, "h", procSet, owner, func(string, ...any) {}); err != nil {
			t.Fatal(err)
		}
	}
	// 41: old production → stopped; 42: production; 43: in flight.
	plan := process.PlanFor([]process.Deployment{
		{ID: 41, Ready: true, Set: procSet},
		{ID: 42, Ready: true, Production: true, Set: procSet},
		{ID: 43, InFlight: true, Set: procSet},
	}, map[string]int{"bot": 4})
	if err := k.ApplyProcesses(ctx, "blog", plan); err != nil {
		t.Fatal(err)
	}
	replicas := func(d store.Deployment, w string) int32 {
		got, err := cs.AppsV1().Deployments(ns).Get(ctx, WorkerName(d, w), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return *got.Spec.Replicas
	}
	suspended := func(d store.Deployment) bool {
		got, err := cs.BatchV1().CronJobs(ns).Get(ctx, CronName(d, "cleanup"), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return *got.Spec.Suspend
	}
	if replicas(deps[0], "queue") != 0 || replicas(deps[0], "bot") != 0 || !suspended(deps[0]) {
		t.Error("old generation still runs")
	}
	if replicas(deps[1], "queue") != 2 || replicas(deps[1], "bot") != 4 || suspended(deps[1]) {
		t.Error("production generation does not run")
	}
	// In flight: as Deploy left it (preview target: queue 2, bot 0, suspended).
	if replicas(deps[2], "queue") != 2 || replicas(deps[2], "bot") != 0 || !suspended(deps[2]) {
		t.Error("in-flight deployment was touched")
	}

	// Rollback to 41: it runs again, 42 stops.
	plan = process.PlanFor([]process.Deployment{
		{ID: 41, Ready: true, Production: true, Set: procSet},
		{ID: 42, Ready: true, Set: procSet},
	}, nil)
	if err := k.ApplyProcesses(ctx, "blog", plan); err != nil {
		t.Fatal(err)
	}
	if replicas(deps[0], "queue") != 2 || suspended(deps[0]) || replicas(deps[1], "queue") != 0 || !suspended(deps[1]) {
		t.Error("rollback did not move the processes")
	}
	if replicas(deps[2], "queue") != 0 { // unknown to the plan now: stopped
		t.Error("deployment missing from the plan still runs")
	}

	// A namespace without process objects is fine.
	if err := k.ApplyProcesses(ctx, "nothing-here", plan); err != nil {
		t.Fatal(err)
	}
}

func TestRunCronAndStatus(t *testing.T) {
	k, cs := newDeployer(t, nil, 5*time.Second)
	ctx := context.Background()
	if err := k.ensureNamespace(ctx, "blog"); err != nil {
		t.Fatal(err)
	}
	owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: name, UID: "u"}
	if err := k.startProcesses(ctx, dep, image, "h", procSet, owner, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.RunCron(ctx, dep, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown cron: %v", err)
	}
	job, err := k.RunCron(ctx, dep, "cleanup")
	if err != nil {
		t.Fatal(err)
	}
	j, err := cs.BatchV1().Jobs(ns).Get(ctx, job, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(job) > 63 || !strings.HasPrefix(job, name+"-c-cleanup-m-") || j.Annotations[annotManualJob] != "manual" ||
		j.OwnerReferences[0].Kind != "CronJob" || j.Labels[LabelProcess] != "cleanup" ||
		j.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("job = %+v", j)
	}
	if _, err := k.RunCron(ctx, dep, "cleanup"); !errors.Is(err, ErrCronRunning) {
		t.Errorf("second run while the first is active: %v", err)
	}

	// A crash-looping worker pod shows up in the status.
	cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: ns, Labels: processSelector(dep, "queue")},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}, metav1.CreateOptions{})
	st, err := k.ProcessStatus(ctx, dep)
	if err != nil {
		t.Fatal(err)
	}
	if q := st.Workers["queue"]; q.Desired != 2 || q.State != "crashing" || q.Reason != "CrashLoopBackOff" {
		t.Errorf("queue = %+v", q)
	}
	if b := st.Workers["bot"]; b.State != "stopped" {
		t.Errorf("bot = %+v", b)
	}
	c := st.Crons["cleanup"]
	if !c.Exists || !c.Suspended || c.LastJob == nil || c.LastJob.Name != job || !c.LastJob.Manual ||
		c.LastJob.Status != "running" || c.Running != 1 {
		t.Errorf("cron = %+v (job %+v)", c, c.LastJob)
	}
	if st.Web != nil {
		t.Errorf("web = %+v, want none (not deployed)", st.Web)
	}

	// The run finishes.
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	cs.BatchV1().Jobs(ns).UpdateStatus(ctx, j, metav1.UpdateOptions{})
	st, _ = k.ProcessStatus(ctx, dep)
	if c := st.Crons["cleanup"]; c.LastJob.Status != "succeeded" || c.Running != 0 {
		t.Errorf("after completion: %+v %+v", c, c.LastJob)
	}
}

func TestProcessLogs(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	ctx := context.Background()
	var buf bytes.Buffer
	var np NoPodsError
	if err := k.ProcessLogs(ctx, dep, "queue", false, 10, &buf); !errors.As(err, &np) {
		t.Fatalf("without pods: %v", err)
	}
	// A web pod of the same deployment is not a queue pod.
	cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns, Labels: selector(dep)}},
		metav1.CreateOptions{})
	if err := k.ProcessLogs(ctx, dep, "queue", false, 10, &buf); !errors.As(err, &np) {
		t.Fatalf("web pod selected for a worker: %v", err)
	}
	cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "q", Namespace: ns, Labels: processLabels(dep, process.KindWorker, "queue")}}, metav1.CreateOptions{})
	if err := k.ProcessLogs(ctx, dep, "queue", false, 10, &buf); err != nil || buf.String() != "fake logs" {
		t.Fatalf("queue logs = %q, %v", buf.String(), err)
	}
	buf.Reset()
	if err := k.ProcessLogs(ctx, dep, "web", false, 10, &buf); err != nil || buf.String() != "fake logs" {
		t.Fatalf("web logs = %q, %v", buf.String(), err)
	}
}

func TestProcessObjectNames(t *testing.T) {
	d := store.Deployment{CommitSHA: sha, Generation: 12}
	long := strings.Repeat("x", process.MaxNameLen)
	if n := CronName(d, long); len(n) > 52 || n != "d-a3f9c1d-12-c-"+long {
		t.Errorf("cron name %q (%d)", n, len(n))
	}
	if got := shortName(strings.Repeat("a", 70), 52); len(got) != 52 || got == shortName(strings.Repeat("a", 71), 52) {
		t.Errorf("shortName = %q", got)
	}
}
