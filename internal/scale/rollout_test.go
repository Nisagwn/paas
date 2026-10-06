package scale

import (
	"context"
	"testing"
	"time"
)

// Faz 21: neither side of an active rollout sleeps, and one that slept
// before the rollout started is woken: its metrics decide the rollout.
func TestRolloutSidesStayAwake(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	e.s.Check(ctx)
	e.clock.advance(11 * time.Minute)
	if r := e.s.Check(ctx); r.Slept != 2 {
		t.Fatalf("want both asleep: %+v", r)
	}

	// A canary from production (2) to the preview-branch deployment (1)
	// starts: both are woken and stay awake however idle.
	e.store.mu.Lock()
	e.store.cands[0].InRollout, e.store.cands[1].InRollout = true, true
	e.store.mu.Unlock()
	if r := e.s.Check(ctx); r.Woken != 2 {
		t.Fatalf("want both woken: %+v", r)
	}
	waitFor(t, "both awake", func() bool {
		return !e.cluster.get(prevKey).Sleeping && !e.cluster.get(prodKey).Sleeping
	})
	e.clock.advance(time.Hour)
	if r := e.s.Check(ctx); r.Slept != 0 {
		t.Fatalf("slept during the rollout: %+v", r)
	}
}
