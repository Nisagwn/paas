package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/worker"
)

// orderPipeline records the stages a deployment went through.
type orderPipeline struct {
	mu    sync.Mutex
	steps []string
	wait  error
}

func (p *orderPipeline) add(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, s)
}

func (p *orderPipeline) Build(ctx context.Context, d store.Deployment, _ store.BuildSettings, log worker.Logger) (worker.BuildResult, error) {
	p.add("build")
	return worker.BuildResult{Image: "registry.local/x@sha256:1"}, nil
}

func (p *orderPipeline) Deploy(context.Context, store.Deployment, string, worker.Logger) error {
	p.add("deploy")
	return nil
}

func (p *orderPipeline) Request(_ context.Context, _ store.Deployment, log worker.Logger) error {
	p.add("request")
	log("==> veritabanı kopyası: db → preview_dev (production'dan kopyalanıyor)")
	return nil
}

func (p *orderPipeline) Wait(context.Context, store.Deployment, worker.Logger) error {
	p.add("wait")
	return p.wait
}

// Faz 22: the database copy is requested before the build (it runs while
// the image builds) and waited for before the deploy; a failed copy fails
// the deployment without deploying it.
func TestDatabasesAroundBuild(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	p := &orderPipeline{}
	w := &worker.Worker{Store: st, Pipeline: p, Databases: p, Domain: "paas.test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("a", 40), "dev", "")
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.steps, ","); got != "request,build,wait,deploy" {
		t.Fatalf("order = %s", got)
	}
	if got, _ := st.GetDeployment(ctx, d.ID); got.Status != store.StatusReady {
		t.Fatalf("status %s", got.Status)
	}

	p.steps, p.wait = nil, errors.New("veritabanı kopyası başarısız (db → preview_dev): ERROR: boom")
	d2, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("b", 40), "dev", "")
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.steps, ","); got != "request,build,wait" {
		t.Fatalf("order after a failed copy = %s", got)
	}
	got, _ := st.GetDeployment(ctx, d2.ID)
	if got.Status != store.StatusFailed || !strings.Contains(got.Error, "veritabanı kopyası başarısız") {
		t.Fatalf("failed copy: %s %q", got.Status, got.Error)
	}
	lines, _ := st.Logs(ctx, d2.ID, 0, 100)
	var all []string
	for _, l := range lines {
		all = append(all, l.Line)
	}
	if !strings.Contains(strings.Join(all, "\n"), "==> veritabanı kopyası: db → preview_dev") {
		t.Errorf("log: %v", all)
	}
}
