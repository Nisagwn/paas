package worker_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/minipaas/internal/store"
	"github.com/nisagwn/minipaas/internal/testdb"
	"github.com/nisagwn/minipaas/internal/worker"
)

// A pipeline that hangs past the timeout must leave the deployment "failed",
// not stuck in "building".
func TestTimeoutMarksFailed(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("a", 40), "main", "")

	w := &worker.Worker{
		Store:    st,
		Pipeline: worker.DryRunPipeline{Step: time.Second},
		Domain:   "paas.test",
		Timeout:  50 * time.Millisecond,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDeployment(ctx, d.ID)
	if got.Status != store.StatusFailed || !strings.Contains(got.Error, "deadline") || got.FinishedAt == nil {
		t.Fatalf("got status=%q error=%q finished=%v", got.Status, got.Error, got.FinishedAt)
	}
}
