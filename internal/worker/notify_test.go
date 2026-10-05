package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/worker"
)

type call struct {
	event  string
	d      store.Deployment
	result worker.Result
	// status of the deployment row at the time of the call.
	rowStatus string
}

// fakeNotifier records calls; err is returned from every call, hang blocks
// until the call's context expires.
type fakeNotifier struct {
	st    *store.Store
	err   error
	hang  bool
	mu    sync.Mutex
	calls []call
}

func (f *fakeNotifier) record(ctx context.Context, c call) error {
	row, _ := f.st.GetDeployment(ctx, c.d.ID)
	c.rowStatus = row.Status
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func (f *fakeNotifier) DeploymentStarted(ctx context.Context, d store.Deployment) error {
	return f.record(ctx, call{event: "started", d: d})
}

func (f *fakeNotifier) DeploymentFinished(ctx context.Context, d store.Deployment, r worker.Result) error {
	return f.record(ctx, call{event: "finished", d: d, result: r})
}

type failingPipeline struct{ worker.DryRunPipeline }

func (failingPipeline) Build(context.Context, store.Deployment, store.BuildSettings, worker.Logger) (worker.BuildResult, error) {
	return worker.BuildResult{}, errors.New("npm ci: exit status 1")
}

func newNotifyWorker(st *store.Store, p worker.Pipeline, n worker.Notifier) *worker.Worker {
	return &worker.Worker{
		Store: st, Pipeline: p, Domain: "paas.test", Notifier: n,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestNotifierSuccess(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	sha := strings.Repeat("b", 40)
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, sha, "main", "")

	n := &fakeNotifier{st: st}
	if _, err := newNotifyWorker(st, worker.DryRunPipeline{}, n).ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.calls) != 2 || n.calls[0].event != "started" || n.calls[1].event != "finished" {
		t.Fatalf("calls = %+v", n.calls)
	}
	if c := n.calls[0]; c.d.ID != d.ID || c.d.Repo != "nisagwn/blog" || c.rowStatus != store.StatusBuilding {
		t.Fatalf("started: %+v", c)
	}
	want := worker.Result{
		Status:        store.StatusReady,
		URL:           "https://bbbbbbb-blog.paas.test",
		PreviewURL:    "https://main-blog.paas.test",
		ProductionURL: "https://blog.paas.test",
	}
	if c := n.calls[1]; c.result != want || c.rowStatus != store.StatusReady {
		t.Fatalf("finished: %+v, want %+v", c, want)
	}
}

func TestNotifierFailure(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("c", 40), "feature/x", "")

	n := &fakeNotifier{st: st}
	if _, err := newNotifyWorker(st, failingPipeline{}, n).ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.calls) != 2 {
		t.Fatalf("calls = %+v", n.calls)
	}
	c := n.calls[1]
	if c.event != "finished" || c.result.Status != store.StatusFailed || c.rowStatus != store.StatusFailed ||
		c.result.Error != "build: npm ci: exit status 1" || c.result.ProductionURL != "" ||
		c.result.URL != "https://ccccccc-blog.paas.test" {
		t.Fatalf("finished: %+v", c)
	}
	if got, _ := st.GetDeployment(ctx, d.ID); got.Status != store.StatusFailed {
		t.Fatalf("status = %q", got.Status)
	}
}

// A failing or hanging notifier never changes the deployment's outcome; it
// leaves a WARNING line in the deployment log.
func TestNotifierErrorsAreLogged(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	d1, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("d", 40), "main", "")
	d2, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("e", 40), "main", "")

	w := newNotifyWorker(st, worker.DryRunPipeline{}, &fakeNotifier{st: st, err: errors.New("github: 403 Resource not accessible")})
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}

	w.Notifier = &fakeNotifier{st: st, hang: true}
	w.NotifyTimeout = 50 * time.Millisecond
	start := time.Now()
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("hanging notifier was not cut off by NotifyTimeout")
	}

	for _, id := range []int64{d1.ID, d2.ID} {
		got, _ := st.GetDeployment(ctx, id)
		if got.Status != store.StatusReady {
			t.Fatalf("deployment %d: status %q", id, got.Status)
		}
		lines, _ := st.Logs(ctx, id, 0, 1000)
		var warnings int
		for _, l := range lines {
			if strings.HasPrefix(l.Line, "WARNING: notification failed") {
				warnings++
			}
		}
		if warnings != 2 { // started + finished
			t.Fatalf("deployment %d: %d warning lines in %+v", id, warnings, lines)
		}
	}
}
