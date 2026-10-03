package deploy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"

	"github.com/nisagwn/minipaas/internal/worker"
)

// fatalReasons are container waiting reasons that will not fix themselves,
// so the rollout fails at once instead of running into the timeout.
// ErrImagePull is left out: it turns into ImagePullBackOff if it persists.
var fatalReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImageNeverPull":          true,
	"InvalidImageName":           true,
	"CrashLoopBackOff":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
}

// logTail is how many container log lines a crash report includes.
const logTail = 20

// waitReady polls until the Deployment has all replicas available, streaming
// pod state changes to log. It fails fast on fatal pod states and gives up
// after RolloutTimeout.
func (k *Kubernetes) waitReady(ctx context.Context, ns, name string, log worker.Logger) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, k.cfg.RolloutTimeout)
	defer cancel()
	log("==> waiting for rollout (timeout %s)", k.cfg.RolloutTimeout)

	w := &rollout{k: k, ns: ns, name: name, log: log, seen: map[string]string{}, available: -1}
	tick := time.NewTicker(k.cfg.PollInterval)
	defer tick.Stop()
	for {
		done, err := w.check(ctx)
		if err != nil {
			return err
		}
		if done {
			log("==> rollout complete in %s", time.Since(start).Round(100*time.Millisecond))
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("not ready after %s (deadline exceeded): %s",
					time.Since(start).Round(time.Second), w.last)
			}
			return ctx.Err()
		case <-tick.C:
		}
	}
}

type rollout struct {
	k         *Kubernetes
	ns, name  string
	log       worker.Logger
	seen      map[string]string // pod → last logged state
	available int32
	last      string // latest status, reported on timeout
}

// check returns done=true once the rollout is complete and a non-nil error
// only for failures that waiting will not fix. API errors are retried.
func (r *rollout) check(ctx context.Context) (bool, error) {
	dep, err := r.k.client.AppsV1().Deployments(r.ns).Get(ctx, r.name, metav1.GetOptions{})
	if err != nil {
		r.last = "get deployment: " + err.Error()
		return false, nil
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	st := dep.Status
	if st.AvailableReplicas != r.available {
		r.available = st.AvailableReplicas
		r.log("    %d/%d replicas available", st.AvailableReplicas, want)
	}
	r.last = fmt.Sprintf("%d/%d replicas available", st.AvailableReplicas, want)

	for _, c := range st.Conditions {
		switch {
		// "status unknown for quota" right after the namespace is created is
		// transient; an exceeded quota is not.
		case c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue:
			if strings.Contains(c.Message, "exceeded quota") {
				return false, fmt.Errorf("cannot create pods: %s", c.Message)
			}
			r.last += "; " + c.Message
		case c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse &&
			c.Reason == "ProgressDeadlineExceeded":
			return false, fmt.Errorf("rollout stalled: %s", c.Message)
		}
	}
	if st.ObservedGeneration >= dep.Generation && st.UpdatedReplicas >= want &&
		st.AvailableReplicas >= want && st.Replicas == st.UpdatedReplicas {
		return true, nil
	}
	return false, r.checkPods(ctx, dep)
}

func (r *rollout) checkPods(ctx context.Context, dep *appsv1.Deployment) error {
	pods, err := r.k.client.CoreV1().Pods(r.ns).List(ctx, metav1.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String(),
	})
	if err != nil {
		return nil // retried on the next tick
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	for i := range pods.Items {
		p := &pods.Items[i]
		state := podState(p)
		if r.seen[p.Name] != state {
			r.seen[p.Name] = state
			r.log("    pod %s: %s", p.Name, state)
		}
		r.last += "; pod " + p.Name + ": " + state
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && fatalReasons[w.Reason] {
				return r.fail(ctx, p, cs)
			}
		}
	}
	return nil
}

// fail builds the error for a pod that will not recover, with the tail of
// the container's log when it got far enough to write one.
func (r *rollout) fail(ctx context.Context, p *corev1.Pod, cs corev1.ContainerStatus) error {
	w := cs.State.Waiting
	msg := fmt.Sprintf("pod %s: %s", p.Name, w.Reason)
	if w.Message != "" {
		msg += ": " + w.Message
	}
	if w.Reason == "CreateContainerConfigError" && strings.Contains(w.Message, "runAsNonRoot") {
		msg += " (hint: the image must run as a numeric non-root USER, e.g. \"USER 1000:1000\"; " +
			"or set MINIPAAS_APP_RUN_AS_NON_ROOT=false)"
	}
	if w.Reason == "CrashLoopBackOff" {
		if t := cs.LastTerminationState.Terminated; t != nil {
			msg += fmt.Sprintf(" (exit code %d)", t.ExitCode)
		}
		if lines := r.tailLogs(ctx, p.Name, cs.Name); len(lines) > 0 {
			r.log("    last %d log line(s) of %s:", len(lines), p.Name)
			for _, l := range lines {
				r.log("    | %s", l)
			}
			if len(lines) > 5 {
				lines = lines[len(lines)-5:]
			}
			msg += "; last log lines: " + strings.Join(lines, " ⏎ ")
		}
	}
	return errors.New(msg)
}

// tailLogs returns the last lines of the previous (crashed) container, or of
// the current one if there is no previous instance. Best effort.
func (r *rollout) tailLogs(ctx context.Context, pod, container string) []string {
	for _, previous := range []bool{true, false} {
		raw, err := r.k.client.CoreV1().Pods(r.ns).GetLogs(pod, &corev1.PodLogOptions{
			Container: container, TailLines: ptr(int64(logTail)), Previous: previous,
		}).DoRaw(ctx)
		if err != nil {
			continue
		}
		if s := strings.TrimRight(string(raw), "\n"); s != "" {
			return strings.Split(s, "\n")
		}
	}
	return nil
}

// podState is a one-line summary such as "Pending (ContainerCreating)" or
// "Running, ready".
func podState(p *corev1.Pod) string {
	s := string(p.Status.Phase)
	if s == "" {
		s = "Pending"
	}
	var details []string
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason != "" {
			details = append(details, c.Reason+": "+c.Message)
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		switch {
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
			details = append(details, cs.State.Waiting.Reason)
		case cs.State.Terminated != nil:
			details = append(details, fmt.Sprintf("%s, exit code %d", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode))
		case cs.State.Running != nil && cs.Ready:
			details = append(details, "ready")
		case cs.State.Running != nil:
			details = append(details, "not ready")
		}
		if cs.RestartCount > 0 {
			details = append(details, fmt.Sprintf("%d restart(s)", cs.RestartCount))
		}
	}
	if len(details) > 0 {
		s += " (" + strings.Join(details, ", ") + ")"
	}
	return s
}
