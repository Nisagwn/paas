package deploy

// Faz 27: browser terminal. Terminal lists the pods of a deployment (web
// and, Faz 20, worker and cron pods) and runs a shell in one of them with a
// TTY through the pods/exec subresource. client-go's remotecommand speaks
// the WebSocket exec protocol (v5.channel.k8s.io) and falls back to SPDY on
// API servers that do not offer it.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/terminal"
)

// Terminal implements terminal.Cluster.
type Terminal struct {
	client kubernetes.Interface
	// urls builds exec URLs (the core/v1 REST client).
	urls rest.Interface
	// executor opens the exec stream of a URL; replaced in tests.
	executor func(u *url.URL) (remotecommand.Executor, error)
}

var _ terminal.Cluster = (*Terminal)(nil)

// NewTerminal connects like NewClient, without the per-request timeout:
// an exec stream lasts as long as the session.
func NewTerminal(kubeconfig string) (*Terminal, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = 0
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Terminal{
		client: client, urls: client.CoreV1().RESTClient(),
		executor: func(u *url.URL) (remotecommand.Executor, error) { return newExecutor(cfg, u) },
	}, nil
}

// Pods returns the deployment's pods: web pods (selected like the web
// Service) and process pods (paas/process-of, Faz 20). Pods being deleted
// are left out.
func (t *Terminal) Pods(ctx context.Context, d store.Deployment) ([]terminal.Pod, error) {
	ns := naming.Namespace(d.AppName)
	var out []terminal.Pod
	for _, sel := range []map[string]string{
		selector(d),
		{LabelApp: d.AppName, LabelProcessOf: fmt.Sprint(d.ID)},
	} {
		list, err := t.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: k8slabels.SelectorFromSet(sel).String(),
		})
		if err != nil {
			return nil, fmt.Errorf("list pods of %s/%s: %w", ns, d.ObjectName(), err)
		}
		for _, p := range list.Items {
			if p.DeletionTimestamp != nil {
				continue
			}
			out = append(out, podInfo(&p))
		}
	}
	return out, nil
}

func podInfo(p *corev1.Pod) terminal.Pod {
	proc := p.Labels[LabelProcess]
	if proc == "" {
		proc = terminal.WebProcess
	}
	tp := terminal.Pod{Name: p.Name, Process: proc, Phase: string(p.Status.Phase), Created: p.CreationTimestamp.Time}
	if p.Status.Phase == corev1.PodRunning {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == ContainerName {
				// Workers and cron runs have no readiness probe: running is enough.
				tp.Ready = cs.Ready || (cs.State.Running != nil && p.Labels[LabelProcessOf] != "")
			}
		}
	}
	return tp
}

// Exec runs req.Command in the pod's app container with a TTY.
func (t *Terminal) Exec(ctx context.Context, req terminal.ExecRequest) error {
	container := req.Container
	if container == "" {
		container = ContainerName
	}
	u := t.urls.Post().Resource("pods").Namespace(req.Namespace).Name(req.Pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container, Command: req.Command,
			Stdin: true, Stdout: true, Stderr: false, TTY: true, // with a TTY stderr is merged into stdout
		}, scheme.ParameterCodec).URL()
	exec, err := t.executor(u)
	if err != nil {
		return err
	}
	var queue remotecommand.TerminalSizeQueue
	if req.Resize != nil {
		queue = sizeQueue{req.Resize}
	}
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin: req.Stdin, Stdout: req.Stdout, Tty: true, TerminalSizeQueue: queue,
	})
	return execError(err)
}

// newExecutor prefers the WebSocket protocol and falls back to SPDY, like
// kubectl exec.
func newExecutor(cfg *rest.Config, u *url.URL) (remotecommand.Executor, error) {
	spdy, err := remotecommand.NewSPDYExecutor(cfg, "POST", u)
	if err != nil {
		return nil, err
	}
	ws, err := remotecommand.NewWebSocketExecutor(cfg, "GET", u.String())
	if err != nil {
		return nil, err
	}
	return remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
}

// sizeQueue adapts terminal.SizeQueue to remotecommand.
type sizeQueue struct{ q terminal.SizeQueue }

func (s sizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := s.q.Next()
	if !ok {
		return nil
	}
	return &remotecommand.TerminalSize{Width: size.Cols, Height: size.Rows}
}

// execError keeps exit statuses (they implement ExitStatus() int) and turns
// a missing /bin/sh into terminal.ErrNoShell. The runtime reports it as,
// e.g., `exec: "/bin/sh": stat /bin/sh: no such file or directory`
// (containerd) or `executable file not found in $PATH`.
func execError(err error) error {
	if err == nil {
		return nil
	}
	var ee interface{ ExitStatus() int }
	if errors.As(err, &ee) {
		return err
	}
	if IsNoShell(err.Error()) {
		return fmt.Errorf("%w: %v", terminal.ErrNoShell, err)
	}
	return err
}

// IsNoShell reports whether an exec error message says /bin/sh is missing.
func IsNoShell(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "/bin/sh") &&
		(strings.Contains(m, "no such file or directory") || strings.Contains(m, "not found") ||
			strings.Contains(m, "executable file not found"))
}
