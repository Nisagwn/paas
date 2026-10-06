package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func intp(n int) *int { return &n }

func TestProcessesRoundTripAndCopy(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "")

	// Built before Faz 20: web only.
	got, err := st.DeploymentProcesses(ctx, d.ID)
	if err != nil || !reflect.DeepEqual(got, process.Default()) {
		t.Fatalf("default = %+v, %v", got, err)
	}

	set := process.Set{
		Source:  "paas.yaml",
		Workers: []process.Worker{{Name: "queue", Command: "node q.js", Exec: []string{"sh", "-c", "node q.js"}, Replicas: 2, Previews: true}},
		Crons:   []process.Cron{{Name: "cleanup", Schedule: "@daily", Command: "node c.js", Exec: []string{"sh", "-c", "node c.js"}}},
	}
	if err := st.SetProcesses(ctx, d.ID, set); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProcesses(ctx, d.ID, set); err != nil { // a retried build
		t.Fatal(err)
	}
	if got, _ = st.DeploymentProcesses(ctx, d.ID); !reflect.DeepEqual(got, set) {
		t.Fatalf("round trip = %+v", got)
	}

	// A redeploy reusing the image gets the image's processes.
	if err := st.SetImage(ctx, d.ID, "img@sha256:1"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkReady(ctx, d, nil); err != nil {
		t.Fatal(err)
	}
	d, _ = st.GetDeployment(ctx, d.ID)
	cp, err := st.CopyDeployment(ctx, d, store.CopyOptions{Origin: store.OriginRedeploy, Target: store.EnvProduction, ReuseImage: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = st.DeploymentProcesses(ctx, cp.ID); !reflect.DeepEqual(got, set) {
		t.Fatalf("copy = %+v", got)
	}
	// A rebuild gets its set from the build instead.
	rb, err := st.CopyDeployment(ctx, d, store.CopyOptions{Origin: store.OriginRedeploy, Target: store.EnvProduction})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = st.DeploymentProcesses(ctx, rb.ID); !got.Empty() {
		t.Fatalf("rebuild copied the set: %+v", got)
	}
}

func TestProcessReplicaOverrides(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	if got, err := st.ProcessReplicas(ctx, app.ID); err != nil || len(got) != 0 {
		t.Fatalf("none = %v, %v", got, err)
	}
	if err := st.SetProcessReplicas(ctx, app.ID, "queue", intp(3)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProcessReplicas(ctx, app.ID, "queue", intp(0)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProcessReplicas(ctx, app.ID, "bot", intp(1)); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ProcessReplicas(ctx, app.ID); !reflect.DeepEqual(got, map[string]int{"queue": 0, "bot": 1}) {
		t.Fatalf("overrides = %v", got)
	}
	if err := st.SetProcessReplicas(ctx, app.ID, "bot", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ProcessReplicas(ctx, app.ID); !reflect.DeepEqual(got, map[string]int{"queue": 0}) {
		t.Fatalf("after reset = %v", got)
	}
	for _, bad := range []struct {
		name string
		n    int
	}{{"queue", 11}, {"queue", -1}, {"web", 1}, {"Bad", 1}} {
		if err := st.SetProcessReplicas(ctx, app.ID, bad.name, intp(bad.n)); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s=%d: %v, want ErrInvalid", bad.name, bad.n, err)
		}
	}
	if err := st.SetProcessReplicas(ctx, 999999, "queue", intp(1)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown app: %v", err)
	}
}

// AppProcesses reports every deployment that may run processes with the
// role its aliases give it; rollback moves the production role.
func TestAppProcesses(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	set := process.Set{Workers: []process.Worker{{Name: "w", Command: "x", Replicas: 1}}}
	bot := process.Set{NoWeb: true, Workers: set.Workers}

	ready := func(n int, branch string, s process.Set) store.Deployment {
		d, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(n), branch, "")
		if err := st.SetProcesses(ctx, d.ID, s); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkReady(ctx, d, aliasesFor(d)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d1 := ready(1, "main", set)
	d2 := ready(2, "main", bot)
	d3 := ready(3, "feature", set)
	d4, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(4), "main", "")
	st.SetProcesses(ctx, d4.ID, set)
	st.EnqueueDeployment(ctx, app.ID, sha(5), "main", "") // no process row: not listed

	deps, overrides, err := st.AppProcesses(ctx, app.ID)
	if err != nil || len(overrides) != 0 {
		t.Fatal(err, overrides)
	}
	type row struct {
		id                                   int64
		ready, inFlight, production, preview bool
		web                                  bool
	}
	var got []row
	for _, d := range deps {
		got = append(got, row{d.ID, d.Ready, d.InFlight, d.Production, d.Preview, d.Set.HasWeb()})
	}
	want := []row{
		{d1.ID, true, false, false, false, true},
		{d2.ID, true, false, true, false, false}, // production branch: never a preview
		{d3.ID, true, false, false, true, true},
		{d4.ID, false, true, false, false, true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deployments:\n got %+v\nwant %+v", got, want)
	}

	webless, err := st.WeblessDeployments(ctx, app.ID)
	if err != nil || !reflect.DeepEqual(webless, map[int64]bool{d2.ID: true}) {
		t.Fatalf("webless = %v, %v", webless, err)
	}
	prod, err := st.ProductionAliasTarget(ctx, app.ID)
	if err != nil || prod.ID != d2.ID {
		t.Fatalf("production = %d, %v", prod.ID, err)
	}

	if _, err := st.Rollback(ctx, app.ID, d1.ID); err != nil {
		t.Fatal(err)
	}
	deps, _, _ = st.AppProcesses(ctx, app.ID)
	if !deps[0].Production || deps[1].Production {
		t.Fatalf("after rollback: %+v", deps[:2])
	}

	other := mustApp(t, st, "other")
	if _, err := st.ProductionAliasTarget(ctx, other.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no production: %v", err)
	}
}

func aliasesFor(d store.Deployment) []store.AliasSpec {
	out := []store.AliasSpec{{Hostname: d.Branch + "-blog.paas.test", Kind: store.AliasPreview, Branch: d.Branch}}
	if d.Branch == "main" {
		out = append(out, store.AliasSpec{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: d.Branch})
	}
	return out
}
