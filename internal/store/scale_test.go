package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func TestScaleCandidatesAndSleeping(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")

	m1 := deploy(t, st, app, 1, "main")
	m2 := deploy(t, st, app, 2, "main") // production now
	f1 := deploy(t, st, app, 3, "feature")
	// Queued and retired deployments are not candidates.
	if _, _, err := st.EnqueueDeployment(ctx, app.ID, sha(4), "other", ""); err != nil {
		t.Fatal(err)
	}

	cands, err := st.ScaleCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]store.ScaleCandidate{}
	for _, c := range cands {
		got[c.DeploymentID] = c
	}
	if len(got) != 3 || !got[m2.ID].Production || got[m1.ID].Production || got[f1.ID].Production ||
		got[m2.ID].ScaleProduction || got[f1.ID].AppName != "blog" || got[f1.ID].CommitSHA != sha(3) ||
		got[f1.ID].FinishedAt.IsZero() {
		t.Fatalf("candidates = %+v", cands)
	}

	// Sleeping state round trip; a second "sleeping" keeps the first time.
	if err := st.SetSleeping(ctx, f1.ID, true); err != nil {
		t.Fatal(err)
	}
	d, _ := st.GetDeployment(ctx, f1.ID)
	if d.SleepingSince == nil {
		t.Fatal("sleeping_since not set")
	}
	first := *d.SleepingSince
	st.SetSleeping(ctx, f1.ID, true)
	if d, _ = st.GetDeployment(ctx, f1.ID); !d.SleepingSince.Equal(first) {
		t.Fatalf("sleeping_since moved: %v → %v", first, d.SleepingSince)
	}
	st.SetSleeping(ctx, f1.ID, false)
	if d, _ = st.GetDeployment(ctx, f1.ID); d.SleepingSince != nil {
		t.Fatal("sleeping_since not cleared")
	}

	// Per-app production setting.
	if on, err := st.ScaleToZeroProduction(ctx, app.ID); err != nil || on {
		t.Fatalf("default = %v %v", on, err)
	}
	if err := st.SetScaleToZeroProduction(ctx, app.ID, true); err != nil {
		t.Fatal(err)
	}
	if on, _ := st.ScaleToZeroProduction(ctx, app.ID); !on {
		t.Fatal("setting not saved")
	}
	cands, _ = st.ScaleCandidates(ctx)
	for _, c := range cands {
		if !c.ScaleProduction {
			t.Fatalf("candidate without app setting: %+v", c)
		}
	}
	if err := st.SetScaleToZeroProduction(ctx, 9999, true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown app: %v", err)
	}
	if _, err := st.ScaleToZeroProduction(ctx, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown app: %v", err)
	}
}
