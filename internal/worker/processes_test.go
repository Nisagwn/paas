package worker_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/worker"
)

// processPipeline builds an image with a process set and records the
// deployments it deploys.
type processPipeline struct {
	set      process.Set
	deployed []int64
}

func (p *processPipeline) Build(_ context.Context, d store.Deployment, _ store.BuildSettings, _ worker.Logger) (worker.BuildResult, error) {
	set := p.set
	return worker.BuildResult{Image: "reg/" + d.AppName + "@sha256:x", Processes: &set}, nil
}

func (p *processPipeline) Deploy(_ context.Context, d store.Deployment, _ string, _ worker.Logger) error {
	p.deployed = append(p.deployed, d.ID)
	return nil
}

// The worker stores the built commit's process set before deploying, and
// a redeploy that reuses the image keeps it.
func TestWorkerStoresProcesses(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("a", 40), "main", "")
	set := process.Set{Source: "paas.yaml", NoWeb: true, Workers: []process.Worker{{Name: "bot", Command: "python bot.py", Replicas: 1}}}
	p := &processPipeline{set: set}
	w := newWorker(st, p)
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := st.DeploymentProcesses(ctx, d.ID); err != nil || !reflect.DeepEqual(got, set) {
		t.Fatalf("stored = %+v, %v", got, err)
	}
	d, _ = st.GetDeployment(ctx, d.ID)
	if d.Status != store.StatusReady {
		t.Fatalf("status = %s (%s)", d.Status, d.Error)
	}

	cp, err := st.CopyDeployment(ctx, d, store.CopyOptions{Origin: store.OriginRedeploy, Target: store.EnvProduction, ReuseImage: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.DeploymentProcesses(ctx, cp.ID); !reflect.DeepEqual(got, set) {
		t.Fatalf("redeploy = %+v", got)
	}
	if len(p.deployed) != 2 {
		t.Fatalf("deployed %v", p.deployed)
	}
}
