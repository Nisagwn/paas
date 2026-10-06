package github_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
)

func TestRolloutFinishedStatus(t *testing.T) {
	f := newFake(t)
	n := &github.Notifier{Client: f.c, PublicURL: "https://paas.test"}
	ctx := context.Background()
	d := deployment()
	for _, r := range []store.Rollout{
		{Mode: store.RolloutCanary, State: store.RolloutPromoted},
		{Mode: store.RolloutCanary, State: store.RolloutRolledBack, Reason: "5xx oranı %30.0, eşik %5.0 (12/40 istek)"},
		{Mode: store.RolloutGuarded, State: store.RolloutAborted, Reason: "metrikler alınamadı"},
		{Mode: store.RolloutCanary, State: store.RolloutRunning}, // not final: nothing sent
	} {
		r.ToDeploymentID = d.ID
		if err := n.RolloutFinished(ctx, d, r); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.statuses) != 3 {
		t.Fatalf("statuses = %+v", f.statuses)
	}
	want := []struct{ state, desc string }{
		{"success", "Canary tamamlandı"},
		{"failure", "Canary geri alındı: 5xx oranı %30.0"},
		{"error", "İzleme durduruldu: metrikler alınamadı"},
	}
	for i, w := range want {
		s := f.statuses[i]
		if s.State != w.state || !strings.HasPrefix(s.Description, w.desc) || s.Context != "paas/deploy/rollout" ||
			s.TargetURL != "https://paas.test/deployments/42" {
			t.Errorf("status %d = %+v, want %s %q", i, s, w.state, w.desc)
		}
	}
}
