package deploy

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/nisagwn/paas/internal/terminal"
)

// fakeExecutor plays the API server's exec stream.
type fakeExecutor struct {
	opts  remotecommand.StreamOptions
	sizes []remotecommand.TerminalSize
	err   error
}

func (f *fakeExecutor) Stream(opts remotecommand.StreamOptions) error {
	return f.StreamWithContext(context.Background(), opts)
}

func (f *fakeExecutor) StreamWithContext(_ context.Context, opts remotecommand.StreamOptions) error {
	f.opts = opts
	if opts.TerminalSizeQueue != nil {
		if s := opts.TerminalSizeQueue.Next(); s != nil {
			f.sizes = append(f.sizes, *s)
		}
	}
	in, _ := io.ReadAll(opts.Stdin)
	opts.Stdout.Write([]byte("out:" + string(in)))
	return f.err
}

type oneSize struct{ done bool }

func (q *oneSize) Next() (terminal.Size, bool) {
	if q.done {
		return terminal.Size{}, false
	}
	q.done = true
	return terminal.Size{Cols: 120, Rows: 40}, true
}

func newTestTerminal(t *testing.T, exec *fakeExecutor) (*Terminal, *fake.Clientset, *url.URL) {
	t.Helper()
	cs := fake.NewClientset()
	// A real REST client builds the exec URL; nothing is dialled.
	real, err := kubernetes.NewForConfig(&rest.Config{Host: "https://k8s.test:6443"})
	if err != nil {
		t.Fatal(err)
	}
	var got url.URL
	return &Terminal{client: cs, urls: real.CoreV1().RESTClient(), executor: func(u *url.URL) (remotecommand.Executor, error) {
		got = *u
		return exec, nil
	}}, cs, &got
}

func TestTerminalPods(t *testing.T) {
	term, cs, _ := newTestTerminal(t, nil)
	ctx := context.Background()
	now := time.Now()
	running := func(ready bool, state bool) corev1.PodStatus {
		cs := corev1.ContainerStatus{Name: ContainerName, Ready: ready}
		if state {
			cs.State.Running = &corev1.ContainerStateRunning{}
		}
		return corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}}
	}
	deleting := metav1.NewTime(now)
	for _, p := range []*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: name + "-web", Namespace: ns, CreationTimestamp: metav1.NewTime(now),
			Labels: map[string]string{LabelApp: "blog", LabelDeploymentID: "42"}}, Status: running(true, true)},
		{ObjectMeta: metav1.ObjectMeta{Name: name + "-starting", Namespace: ns,
			Labels: map[string]string{LabelApp: "blog", LabelDeploymentID: "42"}}, Status: corev1.PodStatus{Phase: corev1.PodPending}},
		{ObjectMeta: metav1.ObjectMeta{Name: name + "-old", Namespace: ns, DeletionTimestamp: &deleting, Finalizers: []string{"x"},
			Labels: map[string]string{LabelApp: "blog", LabelDeploymentID: "42"}}, Status: running(true, true)},
		{ObjectMeta: metav1.ObjectMeta{Name: name + "-w-worker-1", Namespace: ns,
			Labels: map[string]string{LabelApp: "blog", LabelProcessOf: "42", LabelProcess: "worker"}}, Status: running(false, true)},
		{ObjectMeta: metav1.ObjectMeta{Name: "d-other-web", Namespace: ns,
			Labels: map[string]string{LabelApp: "blog", LabelDeploymentID: "43"}}, Status: running(true, true)},
	} {
		if _, err := cs.CoreV1().Pods(ns).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	pods, err := term.Pods(ctx, dep)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]terminal.Pod{}
	for _, p := range pods {
		got[p.Name] = p
	}
	if len(got) != 3 {
		t.Fatalf("pods %+v, want web, starting and worker of deployment 42", pods)
	}
	if p := got[name+"-web"]; p.Process != "web" || !p.Ready {
		t.Errorf("web pod %+v", p)
	}
	if p := got[name+"-starting"]; p.Ready {
		t.Errorf("pending pod is ready: %+v", p)
	}
	// Workers have no readiness probe: running counts.
	if p := got[name+"-w-worker-1"]; p.Process != "worker" || !p.Ready {
		t.Errorf("worker pod %+v", p)
	}
}

func TestTerminalExec(t *testing.T) {
	fe := &fakeExecutor{}
	term, _, u := newTestTerminal(t, fe)
	var out strings.Builder
	err := term.Exec(context.Background(), terminal.ExecRequest{
		Namespace: ns, Pod: name + "-web", Command: terminal.DefaultShell,
		Stdin: strings.NewReader("ls"), Stdout: &out, Resize: &oneSize{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "out:ls" {
		t.Fatalf("stdout %q", out.String())
	}
	if u.Path != "/api/v1/namespaces/app-blog/pods/d-a3f9c1d-web/exec" {
		t.Fatalf("exec path %q", u.Path)
	}
	q := u.Query()
	if q.Get("container") != ContainerName || q.Get("tty") != "true" || q.Get("stdin") != "true" ||
		q.Get("stdout") != "true" || q.Get("stderr") == "true" || q["command"][0] != "/bin/sh" || len(q["command"]) != 3 {
		t.Fatalf("exec query %v", q)
	}
	if !fe.opts.Tty || fe.opts.Stderr != nil || len(fe.sizes) != 1 || fe.sizes[0].Width != 120 || fe.sizes[0].Height != 40 {
		t.Fatalf("stream options %+v sizes %v", fe.opts, fe.sizes)
	}
}

func TestTerminalExecErrors(t *testing.T) {
	for _, c := range []struct {
		name    string
		err     error
		noShell bool
		code    int
	}{
		{"exit status", utilexec.CodeExitError{Err: errors.New("command terminated with exit code 2"), Code: 2}, false, 2},
		{"containerd no shell", errors.New(`failed to exec in container: failed to start exec "x": OCI runtime exec failed: ` +
			`exec failed: unable to start container process: exec: "/bin/sh": stat /bin/sh: no such file or directory: unknown`), true, -1},
		{"path lookup", errors.New(`exec: "/bin/sh": executable file not found in $PATH`), true, -1},
		{"other", errors.New("pods \"x\" is forbidden"), false, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			term, _, _ := newTestTerminal(t, &fakeExecutor{err: c.err})
			err := term.Exec(context.Background(), terminal.ExecRequest{
				Namespace: ns, Pod: "p", Command: []string{"/bin/sh"}, Stdin: strings.NewReader(""), Stdout: io.Discard,
			})
			if errors.Is(err, terminal.ErrNoShell) != c.noShell {
				t.Fatalf("err %v, no shell = %v", err, !c.noShell)
			}
			var ee interface{ ExitStatus() int }
			if c.code >= 0 && (!errors.As(err, &ee) || ee.ExitStatus() != c.code) {
				t.Fatalf("exit status lost: %v", err)
			}
		})
	}
}
