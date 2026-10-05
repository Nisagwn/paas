package deploy

import (
	"context"
	"fmt"
	"io"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// ContainerName is the app container of every deployment pod.
const ContainerName = "app"

// NoPodsError means the deployment has no pod to read logs from (not yet
// scheduled, or removed).
type NoPodsError struct{ Name string }

func (e NoPodsError) Error() string  { return "no pods for deployment " + e.Name }
func (e NoPodsError) NotFound() bool { return true }

// RuntimeLogs copies the container log of the deployment's newest pod to w:
// the last tail lines, then, with follow, new lines until ctx ends or the
// container stops.
//
// Pods are selected by deployment id: a redeploy or promotion of the same
// commit (Faz 17) runs its own pods next to the original's.
func (k *Kubernetes) RuntimeLogs(ctx context.Context, d store.Deployment, follow bool, tail int64, w io.Writer) error {
	ns, name := naming.Namespace(d.AppName), d.ObjectName()
	pods, err := k.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(selector(d)).String(),
	})
	if err != nil {
		return fmt.Errorf("list pods of %s/%s: %w", ns, name, err)
	}
	if len(pods.Items) == 0 {
		return NoPodsError{Name: ns + "/" + name}
	}
	pod := newestPod(pods.Items)

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

// newestPod picks the pod to read: during a restart the replacement pod is
// the live one.
func newestPod(pods []corev1.Pod) string {
	sort.Slice(pods, func(i, j int) bool {
		return pods[j].CreationTimestamp.Before(&pods[i].CreationTimestamp)
	})
	return pods[0].Name
}
