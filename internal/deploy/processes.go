package deploy

// Faz 20: process types. Besides its web process a deployment may run
// workers and cron jobs (internal/process), all from its own image and env
// Secret:
//
//	Deployment d-<sha7>-w-<proc>  worker: no port, Service, Ingress or probe
//	CronJob    d-<sha7>-c-<cron>  cron: concurrencyPolicy Forbid, bounded runs
//
// They are owned by the web Deployment (or, for a deployment without a web
// process, by its env Secret), so Retire removes them with everything else.
// Their pods carry paas/process-of=<deployment id> instead of
// paas/deployment-id, so the web Service, the rollout wait, the activator
// and runtime logs of the web process never select them.
//
// One generation per app: ApplyProcesses (called by the route reconcile
// after every alias change) scales the workers of every other deployment to
// zero and suspends their crons; see process.PlanFor for the rule. Deploy
// starts the workers of a new deployment before it takes its alias and
// checks they stay up; its crons stay suspended until the reconcile turns
// them on, so a schedule never fires twice. Workers of the old and the new
// generation overlap for a few seconds, until the reconcile that follows
// MarkReady stops the old ones.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

// Labels of process objects and their pods.
const (
	LabelProcess     = "paas/process"      // worker or cron name
	LabelProcessKind = "paas/process-kind" // process.KindWorker or process.KindCron
	LabelProcessOf   = "paas/process-of"   // id of the deployment the process belongs to
)

const (
	// workerMinReady: a worker pod counts as up only after running this
	// long without a restart, so one that crashes right after starting
	// fails the deployment instead of slipping through.
	workerMinReady = 10
	// Cron jobs: a missed run is still started within cronStartDeadline
	// (e.g. after a control plane outage), a run is stopped after
	// cronRunDeadline, and a few finished runs are kept for status and logs.
	cronStartDeadline = 300
	cronRunDeadline   = 3600
	cronHistory       = 3
	// annotManualJob marks a run started with RunCron (kubectl uses the same).
	annotManualJob = "cronjob.kubernetes.io/instantiate"
)

// ProcessSource provides process sets and replica overrides (store.Store).
// New picks it up from the EnvSource when that implements it.
type ProcessSource interface {
	DeploymentProcesses(ctx context.Context, deploymentID int64) (process.Set, error)
	ProcessReplicas(ctx context.Context, appID int64) (map[string]int, error)
}

// webWorkloads selects the web Deployments paas manages (Workloads,
// RepointActivator): workers never sleep, scale to zero is for HTTP
// traffic, and they have no Service to point at the activator.
const webWorkloads = LabelManagedBy + "=" + ManagedBy + ",!" + LabelProcess

// ErrCronRunning: a run of the cron job is still active.
var ErrCronRunning = errors.New("a run of this cron job is still active")

func (k *Kubernetes) processSet(ctx context.Context, d store.Deployment) (process.Set, error) {
	if k.Processes == nil {
		return process.Default(), nil
	}
	return k.Processes.DeploymentProcesses(ctx, d.ID)
}

// shortName keeps an object name within max characters; longer names are
// cut and made unique with a hash (like certSecretName).
func shortName(name string, max int) string {
	if len(name) <= max {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:max-9] + "-" + hex.EncodeToString(sum[:])[:8]
}

// WorkerName is the Deployment of a worker process.
func WorkerName(d store.Deployment, proc string) string {
	return shortName(d.ObjectName()+"-w-"+proc, 63)
}

// CronName is the CronJob of a cron process. CronJob names are limited to
// 52 characters (the controller appends an 11-character suffix to jobs).
func CronName(d store.Deployment, cron string) string {
	return shortName(d.ObjectName()+"-c-"+cron, 52)
}

func processLabels(d store.Deployment, kind, proc string) map[string]string {
	l := labels(d)
	delete(l, LabelDeploymentID)
	l[LabelProcessOf] = fmt.Sprint(d.ID)
	l[LabelProcess] = proc
	l[LabelProcessKind] = kind
	return l
}

func processSelector(d store.Deployment, proc string) map[string]string {
	return map[string]string{LabelApp: d.AppName, LabelProcessOf: fmt.Sprint(d.ID), LabelProcess: proc}
}

// processPod derives a process's pod template from the web pod template:
// same image, env, security context and resources; no port or probe; the
// process's command and PAAS_PROCESS.
func (k *Kubernetes) processPod(d store.Deployment, image, envHash, kind, proc string, exec []string) corev1.PodTemplateSpec {
	base := k.deploymentObject(d, image, envHash, nil).Spec.Template
	base.Labels = processLabels(d, kind, proc)
	c := &base.Spec.Containers[0]
	c.Ports, c.ReadinessProbe = nil, nil
	c.Command, c.Args = exec, nil
	c.Env = processEnv(d, proc)
	return base
}

// processEnv is envVars with PAAS_PROCESS set to the process name.
func processEnv(d store.Deployment, proc string) []corev1.EnvVar {
	env := envVars(d)
	for i := range env {
		if env[i].Name == "PAAS_PROCESS" {
			env[i].Value = proc
		}
	}
	return env
}

func secretOwnerRef(s *corev1.Secret) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "v1", Kind: "Secret", Name: s.Name, UID: s.UID}
}

func (k *Kubernetes) workerObject(d store.Deployment, image, envHash string, w process.Worker, replicas int32, owner metav1.OwnerReference) *appsv1.Deployment {
	lbl := processLabels(d, process.KindWorker, w.Name)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: WorkerName(d, w.Name), Namespace: naming.Namespace(d.AppName), Labels: lbl,
			Annotations:     map[string]string{AnnotBranch: d.Branch, AnnotEnvHash: envHash},
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:             ptr(replicas),
			RevisionHistoryLimit: ptr(int32(1)),
			MinReadySeconds:      workerMinReady,
			Selector:             &metav1.LabelSelector{MatchLabels: processSelector(d, w.Name)},
			Template:             k.processPod(d, image, envHash, process.KindWorker, w.Name, w.Exec),
		},
	}
}

func (k *Kubernetes) cronObject(d store.Deployment, image, envHash string, c process.Cron, suspend bool, owner metav1.OwnerReference) *batchv1.CronJob {
	lbl := processLabels(d, process.KindCron, c.Name)
	pod := k.processPod(d, image, envHash, process.KindCron, c.Name, c.Exec)
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: CronName(d, c.Name), Namespace: naming.Namespace(d.AppName), Labels: lbl,
			Annotations:     map[string]string{AnnotBranch: d.Branch, AnnotEnvHash: envHash},
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   c.Schedule,
			Suspend:                    ptr(suspend),
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			StartingDeadlineSeconds:    ptr(int64(cronStartDeadline)),
			SuccessfulJobsHistoryLimit: ptr(int32(cronHistory)),
			FailedJobsHistoryLimit:     ptr(int32(cronHistory)),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: batchv1.JobSpec{
					BackoffLimit:          ptr(int32(1)),
					ActiveDeadlineSeconds: ptr(int64(cronRunDeadline)),
					Template:              pod,
				},
			},
		},
	}
}

func (k *Kubernetes) ensureWorker(ctx context.Context, want *appsv1.Deployment) (*appsv1.Deployment, error) {
	return apply(ctx, k.client.AppsV1().Deployments(want.Namespace), want.Name, want,
		func(have *appsv1.Deployment) bool {
			if equality.Semantic.DeepEqual(have.Spec.Template, want.Spec.Template) &&
				equality.Semantic.DeepEqual(have.Spec.Replicas, want.Spec.Replicas) &&
				equality.Semantic.DeepEqual(have.OwnerReferences, want.OwnerReferences) {
				return false
			}
			have.Labels, have.Annotations, have.OwnerReferences = want.Labels, want.Annotations, want.OwnerReferences
			have.Spec.Replicas, have.Spec.MinReadySeconds = want.Spec.Replicas, want.Spec.MinReadySeconds
			have.Spec.Template = want.Spec.Template // the selector is immutable and unchanged
			return true
		})
}

func (k *Kubernetes) ensureCron(ctx context.Context, want *batchv1.CronJob) (*batchv1.CronJob, error) {
	return apply(ctx, k.client.BatchV1().CronJobs(want.Namespace), want.Name, want,
		func(have *batchv1.CronJob) bool {
			if equality.Semantic.DeepEqual(have.Spec, want.Spec) &&
				equality.Semantic.DeepEqual(have.OwnerReferences, want.OwnerReferences) {
				return false
			}
			have.Labels, have.Annotations, have.OwnerReferences = want.Labels, want.Annotations, want.OwnerReferences
			have.Spec = want.Spec
			return true
		})
}

// startProcesses creates the workers and crons of a deployment being
// deployed. Workers start with the replicas the deployment will run with
// once it takes its alias (production: everything, with the app's
// overrides; preview: processes with previews: true), so a broken worker
// fails the deployment before it goes live. Crons are created suspended.
func (k *Kubernetes) startProcesses(ctx context.Context, d store.Deployment, image, envHash string, set process.Set,
	owner metav1.OwnerReference, log worker.Logger) error {
	if len(set.Workers) == 0 && len(set.Crons) == 0 {
		return nil
	}
	role := process.RolePreview
	var overrides map[string]int
	if d.Target == store.EnvProduction {
		role = process.RoleProduction
		if k.Processes != nil {
			var err error
			if overrides, err = k.Processes.ProcessReplicas(ctx, d.AppID); err != nil {
				return fmt.Errorf("replica overrides: %w", err)
			}
		}
	}
	act := set.Activity(role, overrides)
	ns := naming.Namespace(d.AppName)
	for _, w := range set.Workers {
		n := act.Workers[w.Name]
		if _, err := k.ensureWorker(ctx, k.workerObject(d, image, envHash, w, n, owner)); err != nil {
			return fmt.Errorf("worker %s/%s: %w", ns, WorkerName(d, w.Name), err)
		}
		switch {
		case n > 0:
			log("    worker %s: %d replica(s), %s", w.Name, n, w.Command)
		case role == process.RolePreview && !w.Previews:
			log("    worker %s: not run in previews (set previews: true to run it)", w.Name)
		default:
			log("    worker %s: 0 replicas", w.Name)
		}
	}
	for _, c := range set.Crons {
		if _, err := k.ensureCron(ctx, k.cronObject(d, image, envHash, c, true, owner)); err != nil {
			return fmt.Errorf("cron %s/%s: %w", ns, CronName(d, c.Name), err)
		}
		when := "scheduled once this deployment is live"
		if role == process.RolePreview && !c.Previews {
			when = "not scheduled in previews"
		}
		log("    cron %s (%s, UTC): %s", c.Name, c.Schedule, when)
	}
	return nil
}

// waitWorkers waits until every worker that should run is up (see
// workerMinReady), with the same failure detection as the web rollout:
// crash loops, missing images and an exceeded quota fail at once.
func (k *Kubernetes) waitWorkers(ctx context.Context, d store.Deployment, set process.Set, log worker.Logger) error {
	ns := naming.Namespace(d.AppName)
	for _, w := range set.Workers {
		name := WorkerName(d, w.Name)
		dep, err := k.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("worker %s: %w", w.Name, err)
		}
		if dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 {
			continue
		}
		log("==> waiting for worker %s (up for %ds, timeout %s)", w.Name, workerMinReady, k.cfg.RolloutTimeout)
		if err := k.waitRollout(ctx, ns, name, log); err != nil {
			return fmt.Errorf("worker %s: %w", w.Name, err)
		}
	}
	return nil
}

// waitRollout is waitReady without its log lines.
func (k *Kubernetes) waitRollout(ctx context.Context, ns, name string, log worker.Logger) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, k.cfg.RolloutTimeout)
	defer cancel()
	r := &rollout{k: k, ns: ns, name: name, log: log, seen: map[string]string{}, available: -1}
	tick := time.NewTicker(k.cfg.PollInterval)
	defer tick.Stop()
	for {
		done, err := r.check(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("not up after %s (deadline exceeded): %s", time.Since(start).Round(time.Second), r.last)
			}
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// deployWithoutWeb deploys a deployment that has no web process: its
// workers and crons, owned by the env Secret. It is ready once its workers
// are.
func (k *Kubernetes) deployWithoutWeb(ctx context.Context, d store.Deployment, image, envHash string, secret *corev1.Secret,
	set process.Set, log worker.Logger) error {
	log("    no web process: no Deployment, Service or Ingress for HTTP")
	if err := k.startProcesses(ctx, d, image, envHash, set, secretOwnerRef(secret), log); err != nil {
		return err
	}
	return k.waitWorkers(ctx, d, set, log)
}

// ---- reconcile ----

// ApplyProcesses makes the app's workers and crons match plan (one
// generation per app environment, process.PlanFor). It only scales and
// suspends; objects are created by Deploy and removed by Retire. It is
// idempotent and cheap when nothing changes.
func (k *Kubernetes) ApplyProcesses(ctx context.Context, app string, plan process.Plan) error {
	ns := naming.Namespace(app)
	var errs []error
	dc := k.client.AppsV1().Deployments(ns)
	workers, err := dc.List(ctx, metav1.ListOptions{LabelSelector: LabelProcessKind + "=" + process.KindWorker})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("list workers: %w", err)
	}
	if workers != nil {
		for _, w := range workers.Items {
			id, _ := strconv.ParseInt(w.Labels[LabelProcessOf], 10, 64)
			if plan.Leave(id) {
				continue
			}
			want := plan.Replicas(id, w.Labels[LabelProcess])
			if w.Spec.Replicas != nil && *w.Spec.Replicas == want {
				continue
			}
			err := k.updateDeployment(ctx, ns, w.Name, func(d *appsv1.Deployment) bool {
				if d.Spec.Replicas != nil && *d.Spec.Replicas == want {
					return false
				}
				d.Spec.Replicas = ptr(want)
				return true
			})
			if err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("scale worker %s: %w", w.Name, err))
			}
		}
	}
	cc := k.client.BatchV1().CronJobs(ns)
	crons, err := cc.List(ctx, metav1.ListOptions{LabelSelector: LabelProcessKind + "=" + process.KindCron})
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.Join(append(errs, fmt.Errorf("list crons: %w", err))...)
	}
	if crons != nil {
		for _, c := range crons.Items {
			id, _ := strconv.ParseInt(c.Labels[LabelProcessOf], 10, 64)
			if plan.Leave(id) {
				continue
			}
			suspend := !plan.CronActive(id, c.Labels[LabelProcess])
			if c.Spec.Suspend != nil && *c.Spec.Suspend == suspend {
				continue
			}
			cur := c.DeepCopy()
			cur.Spec.Suspend = ptr(suspend)
			if _, err := cc.Update(ctx, cur, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
				// A conflict is retried by the next reconcile.
				errs = append(errs, fmt.Errorf("suspend cron %s: %w", c.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// ---- status, manual runs, logs ----

// ProcessStatus reads the cluster state of a deployment's processes.
func (k *Kubernetes) ProcessStatus(ctx context.Context, d store.Deployment) (process.Status, error) {
	ns := naming.Namespace(d.AppName)
	st := process.Status{Workers: map[string]process.ReplicaStatus{}, Crons: map[string]process.CronStatus{}}
	of := k8slabels.SelectorFromSet(map[string]string{LabelProcessOf: fmt.Sprint(d.ID)}).String()

	pods, err := k.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: of})
	if err != nil {
		return st, fmt.Errorf("pods: %w", err)
	}
	reasons := map[string]string{} // process → fatal waiting reason
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && fatalReasons[w.Reason] {
				reasons[p.Labels[LabelProcess]] = w.Reason
			}
		}
	}

	if dep, err := k.client.AppsV1().Deployments(ns).Get(ctx, d.ObjectName(), metav1.GetOptions{}); err == nil &&
		dep.Labels[LabelDeploymentID] == fmt.Sprint(d.ID) {
		rs := replicaStatus(dep, "")
		if _, ok := dep.Annotations[AnnotSleeping]; ok {
			rs.State = "sleeping"
		}
		st.Web = &rs
	} else if err != nil && !apierrors.IsNotFound(err) {
		return st, fmt.Errorf("deployment: %w", err)
	}

	workers, err := k.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: of})
	if err != nil {
		return st, fmt.Errorf("workers: %w", err)
	}
	for i := range workers.Items {
		w := &workers.Items[i]
		name := w.Labels[LabelProcess]
		st.Workers[name] = replicaStatus(w, reasons[name])
	}

	crons, err := k.client.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{LabelSelector: of})
	if err != nil {
		return st, fmt.Errorf("cron jobs: %w", err)
	}
	jobs, err := k.client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: of})
	if err != nil {
		return st, fmt.Errorf("jobs: %w", err)
	}
	last := map[string]*batchv1.Job{}
	for i := range jobs.Items {
		j := &jobs.Items[i]
		name := j.Labels[LabelProcess]
		if cur, ok := last[name]; !ok || cur.CreationTimestamp.Before(&j.CreationTimestamp) ||
			(cur.CreationTimestamp.Equal(&j.CreationTimestamp) && cur.Name < j.Name) {
			last[name] = j
		}
	}
	for _, c := range crons.Items {
		name := c.Labels[LabelProcess]
		cs := process.CronStatus{
			Exists: true, Suspended: c.Spec.Suspend != nil && *c.Spec.Suspend, Running: len(c.Status.Active),
			LastScheduleTime: timeOf(c.Status.LastScheduleTime), LastSuccessfulTime: timeOf(c.Status.LastSuccessfulTime),
		}
		if j := last[name]; j != nil {
			cs.LastJob = jobStatus(j)
			if cs.LastJob.Status == "running" && cs.Running == 0 {
				cs.Running = 1 // a manual run
			}
		}
		st.Crons[name] = cs
	}
	return st, nil
}

func timeOf(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

func replicaStatus(dep *appsv1.Deployment, reason string) process.ReplicaStatus {
	rs := process.ReplicaStatus{Desired: 1, Ready: dep.Status.AvailableReplicas, Reason: reason}
	if dep.Spec.Replicas != nil {
		rs.Desired = *dep.Spec.Replicas
	}
	switch {
	case rs.Desired == 0:
		rs.State = "stopped"
	case reason != "":
		rs.State = "crashing"
	case rs.Ready >= rs.Desired:
		rs.State = "running"
	default:
		rs.State = "starting"
	}
	return rs
}

func jobStatus(j *batchv1.Job) *process.JobStatus {
	js := &process.JobStatus{Name: j.Name, Status: "running", Manual: j.Annotations[annotManualJob] == "manual",
		Started: timeOf(j.Status.StartTime), Finished: timeOf(j.Status.CompletionTime)}
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			js.Status = "succeeded"
		case batchv1.JobFailed:
			js.Status = "failed"
			if js.Finished == nil {
				js.Finished = timeOf(&c.LastTransitionTime)
			}
		}
	}
	return js
}

// RunCron starts a run of a deployment's cron job now, from the CronJob's
// own job template (like `kubectl create job --from=cronjob/…`). The run is
// owned by the CronJob, so its history limits clean it up. It returns the
// Job's name; ErrNotFound if the deployment has no such CronJob,
// ErrCronRunning if a run is still active.
func (k *Kubernetes) RunCron(ctx context.Context, d store.Deployment, cron string) (string, error) {
	ns := naming.Namespace(d.AppName)
	cj, err := k.client.BatchV1().CronJobs(ns).Get(ctx, CronName(d, cron), metav1.GetOptions{})
	if apierrors.IsNotFound(err) || (err == nil && cj.Labels[LabelProcessOf] != fmt.Sprint(d.ID)) {
		return "", fmt.Errorf("cron job %s of deployment %d: %w", cron, d.ID, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	jobs, err := k.client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(map[string]string{LabelProcessOf: fmt.Sprint(d.ID), LabelProcess: cron}).String(),
	})
	if err != nil {
		return "", err
	}
	for i := range jobs.Items {
		if jobStatus(&jobs.Items[i]).Status == "running" {
			return "", ErrCronRunning
		}
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        shortName(cj.Name+"-m-"+strconv.FormatInt(time.Now().UnixMilli(), 36), 63),
			Namespace:   ns,
			Labels:      cj.Spec.JobTemplate.Labels,
			Annotations: map[string]string{annotManualJob: "manual"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "CronJob", Name: cj.Name, UID: cj.UID, Controller: ptr(true),
			}},
		},
		Spec: *cj.Spec.JobTemplate.Spec.DeepCopy(),
	}
	got, err := k.client.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return got.Name, nil
}

// ProcessLogs is RuntimeLogs for one process of the deployment: "web" (or
// "") is the web process, a worker's name its newest pod, a cron's name the
// pod of its latest run.
func (k *Kubernetes) ProcessLogs(ctx context.Context, d store.Deployment, proc string, follow bool, tail int64, w io.Writer) error {
	if proc == "" || proc == process.Web {
		return k.RuntimeLogs(ctx, d, follow, tail, w)
	}
	return k.podLogs(ctx, naming.Namespace(d.AppName), proc+" of "+d.ObjectName(),
		processSelector(d, proc), follow, tail, w)
}

// podLogs streams the log of the newest pod matching sel.
func (k *Kubernetes) podLogs(ctx context.Context, ns, what string, sel map[string]string, follow bool, tail int64, w io.Writer) error {
	pods, err := k.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(sel).String(),
	})
	if err != nil {
		return fmt.Errorf("list pods of %s: %w", what, err)
	}
	if len(pods.Items) == 0 {
		return NoPodsError{Name: ns + "/" + what}
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
	})
	pod := pods.Items[0].Name
	opts := &corev1.PodLogOptions{Container: ContainerName, Follow: follow}
	if tail > 0 {
		opts.TailLines = ptr(tail)
	}
	rc, err := k.client.CoreV1().Pods(ns).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		return fmt.Errorf("logs of pod %s/%s: %w", ns, pod, err)
	}
	defer rc.Close()
	if _, err := io.Copy(w, rc); err != nil && ctx.Err() == nil {
		return fmt.Errorf("logs of pod %s/%s: %w", ns, pod, err)
	}
	return nil
}
