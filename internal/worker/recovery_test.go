package worker_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/worker"
)

// A worker that dies mid-deployment stops sending heartbeats. Another
// worker queues the deployment again and finishes it.
func TestRecoverOrphanedDeployment(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("a", 40), "main", "")

	// The "dead" worker: claims the deployment, then never beats.
	if _, err := st.ClaimNext(ctx); err != nil {
		t.Fatal(err)
	}
	w := &worker.Worker{
		Store: st, Pipeline: worker.DryRunPipeline{}, Domain: "paas.test",
		StaleAfter: 100 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if got, _ := w.RecoverStale(ctx); len(got) != 0 {
		t.Fatalf("recovered %v before the heartbeat went stale", got)
	}
	time.Sleep(250 * time.Millisecond)

	got, err := w.RecoverStale(ctx)
	if err != nil || len(got) != 1 || got[0].ID != d.ID || got[0].Status != store.StatusQueued {
		t.Fatalf("recovered %+v, %v; want deployment %d queued again", got, err, d.ID)
	}
	if worked, err := w.ProcessOne(ctx); !worked || err != nil {
		t.Fatalf("ProcessOne: %v %v", worked, err)
	}
	if after, _ := st.GetDeployment(ctx, d.ID); after.Status != store.StatusReady {
		t.Fatalf("status %s after retry, want ready", after.Status)
	}
	logs, _ := st.Logs(ctx, d.ID, 0, 100)
	if len(logs) == 0 || !strings.Contains(logs[0].Line, "queued again") {
		t.Fatalf("first log line = %+v, want the recovery note", logs)
	}
}

// A deployment that keeps killing its worker is not retried forever.
func TestRecoverGivesUpAfterMaxAttempts(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("b", 40), "main", "")
	w := &worker.Worker{
		Store: st, Pipeline: worker.DryRunPipeline{}, Domain: "paas.test",
		StaleAfter: 100 * time.Millisecond, MaxAttempts: 1,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	st.ClaimNext(ctx)
	time.Sleep(250 * time.Millisecond)
	got, err := w.RecoverStale(ctx)
	if err != nil || len(got) != 1 || got[0].Status != store.StatusFailed {
		t.Fatalf("recovered %+v, %v; want failed", got, err)
	}
	after, _ := st.GetDeployment(ctx, d.ID)
	if after.Status != store.StatusFailed || !strings.Contains(after.Error, "worker restarted") {
		t.Fatalf("deployment = %s %q", after.Status, after.Error)
	}
	if worked, _ := w.ProcessOne(ctx); worked {
		t.Fatal("a failed deployment must not be claimed again")
	}
}
