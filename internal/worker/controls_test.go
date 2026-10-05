package worker_test

import (
	"context"
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

// blockingPipeline blocks in the chosen stage until its context ends, and
// counts builds.
type blockingPipeline struct {
	blockIn string // "build" or "deploy"
	started chan int64

	mu     sync.Mutex
	builds int
}

func (p *blockingPipeline) Build(ctx context.Context, d store.Deployment, _ store.BuildSettings, _ worker.Logger) (worker.BuildResult, error) {
	p.mu.Lock()
	p.builds++
	p.mu.Unlock()
	if p.blockIn == "build" {
		p.started <- d.ID
		<-ctx.Done()
		return worker.BuildResult{}, ctx.Err()
	}
	return worker.BuildResult{Image: "reg/" + d.AppName + ":" + d.CommitSHA + "@sha256:built"}, nil
}

func (p *blockingPipeline) Deploy(ctx context.Context, d store.Deployment, _ string, _ worker.Logger) error {
	if p.blockIn == "deploy" {
		p.started <- d.ID
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func newWorker(st *store.Store, p worker.Pipeline) *worker.Worker {
	return &worker.Worker{
		Store: st, Pipeline: p, Domain: "paas.test", Timeout: 10 * time.Second,
		CancelPoll: 20 * time.Millisecond,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// A cancel request reaches a building or deploying run through the
// heartbeat loop; the deployment ends as canceled, without aliases.
func TestCancelInFlight(t *testing.T) {
	for _, stage := range []string{"build", "deploy"} {
		t.Run(stage, func(t *testing.T) {
			st := testdb.Open(t)
			ctx := context.Background()
			app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
			d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("a", 40), "main", "")
			p := &blockingPipeline{blockIn: stage, started: make(chan int64, 1)}
			w := newWorker(st, p)

			done := make(chan error, 1)
			go func() {
				_, err := w.ProcessOne(ctx)
				done <- err
			}()
			select {
			case <-p.started:
			case <-time.After(5 * time.Second):
				t.Fatal("pipeline did not start")
			}
			got, err := st.CancelDeployment(ctx, d.ID)
			want := store.StatusBuilding
			if stage == "deploy" {
				want = store.StatusDeploying
			}
			if err != nil || got.Status != want {
				t.Fatalf("cancel: %+v %v", got, err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("run was not canceled")
			}
			got, _ = st.GetDeployment(ctx, d.ID)
			if got.Status != store.StatusCanceled || got.Error != store.CanceledReason || got.FinishedAt == nil {
				t.Fatalf("after cancel: %+v", got)
			}
			if aliases, _ := st.ListAliases(ctx, app.ID); len(aliases) != 0 {
				t.Fatalf("aliases: %+v", aliases)
			}
			lines, _ := st.Logs(ctx, d.ID, 0, 100)
			if last := lines[len(lines)-1].Line; !strings.Contains(last, "canceled") {
				t.Fatalf("last log line %q", last)
			}
		})
	}
}

// A deployment queued with an image is deployed without a build.
func TestReusedImageSkipsBuild(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	st.EnqueueDeployment(ctx, app.ID, strings.Repeat("b", 40), "feature", "")
	p := &blockingPipeline{}
	w := newWorker(st, p)
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	deps, _ := st.ListDeployments(ctx, app.ID, 10)
	src := deps[0]
	if src.Status != store.StatusReady || p.builds != 1 {
		t.Fatalf("source: %+v builds=%d", src, p.builds)
	}

	cp, err := st.CopyDeployment(ctx, src, store.CopyOptions{Origin: store.OriginRedeploy, Target: store.EnvPreview, ReuseImage: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDeployment(ctx, cp.ID)
	if got.Status != store.StatusReady || got.Image != src.Image || p.builds != 1 {
		t.Fatalf("redeploy: %+v builds=%d", got, p.builds)
	}
	lines, _ := st.Logs(ctx, cp.ID, 0, 100)
	var all []string
	for _, l := range lines {
		all = append(all, l.Line)
	}
	if !strings.Contains(strings.Join(all, "\n"), "reusing image "+src.Image+" (no rebuild)") ||
		!strings.Contains(all[len(all)-1], "ready: https://bbbbbbb-1-blog.paas.test") {
		t.Fatalf("logs: %v", all)
	}
	// The feature preview alias moved to the redeploy.
	aliases, _ := st.ListAliases(ctx, app.ID)
	if len(aliases) != 1 || aliases[0].DeploymentID != cp.ID {
		t.Fatalf("aliases: %+v", aliases)
	}
}

func TestAliasesFollowEnvironment(t *testing.T) {
	type c struct {
		branch, target string
		want           string
	}
	for _, tc := range []c{
		{"main", store.EnvProduction, "preview:main-blog.paas.test production:blog.paas.test"},
		{"feature", store.EnvPreview, "preview:feature-blog.paas.test"},
		// A promoted preview: production only; the branch preview stays.
		{"feature", store.EnvProduction, "production:blog.paas.test"},
	} {
		var got []string
		for _, a := range worker.Aliases(store.Deployment{AppName: "blog", Branch: tc.branch, Target: tc.target}, "main", "paas.test") {
			got = append(got, a.Kind+":"+a.Hostname)
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s/%s: %v, want %s", tc.branch, tc.target, got, tc.want)
		}
	}
}
