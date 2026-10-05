package deploy

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRuntimeLogs(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	ctx := context.Background()

	var buf bytes.Buffer
	err := k.RuntimeLogs(ctx, dep, true, 200, &buf)
	var np NoPodsError
	if !errors.As(err, &np) || !np.NotFound() {
		t.Fatalf("without pods: err = %v, want NoPodsError", err)
	}

	pod := func(name string, age time.Duration, id string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			Labels: map[string]string{LabelApp: "blog", LabelCommit: sha[:40], LabelDeploymentID: id},
		}}
	}
	for _, p := range []*corev1.Pod{
		pod(name+"-old", time.Hour, "42"),
		pod(name+"-new", time.Minute, "42"),
		pod("d-a3f9c1d-1-x", 0, "43"), // a redeploy of the same commit
	} {
		if _, err := cs.CoreV1().Pods(ns).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	if err := k.RuntimeLogs(ctx, dep, true, 200, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "fake logs" {
		t.Fatalf("logs = %q", buf.String())
	}
	// The newest pod of the commit is the one read; other commits are ignored.
	list, _ := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelDeploymentID + "=42"})
	if got := newestPod(list.Items); got != name+"-new" {
		t.Errorf("reads logs of %q, want %q", got, name+"-new")
	}
}
