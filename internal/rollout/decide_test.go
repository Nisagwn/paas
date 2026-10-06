package rollout

import (
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

func ms(v float64) *float64 { return &v }

func sample(req, errs int64, p95 *float64) store.RolloutSample {
	return store.RolloutSample{Requests: req, Errors: errs, P95Ms: p95}
}

func TestDecide(t *testing.T) {
	def := store.DefaultRolloutSettings() // 5 %, +2 points, p95 ×2, min 50
	abs := def
	abs.MaxP95Ms = 300
	step := 5 * time.Minute
	healthy := sample(1000, 5, ms(80))
	cases := []struct {
		name     string
		in       Input
		want     Action
		contains string
	}{
		{"healthy, step running", Input{Elapsed: time.Minute, Canary: sample(200, 1, ms(80)), Baseline: healthy}, Hold, "adım sürüyor"},
		{"healthy, step passed", Input{Elapsed: step, Canary: sample(200, 1, ms(80)), Baseline: healthy}, Advance, "eşikler içinde"},
		{"healthy, last step", Input{Elapsed: step, LastStep: true, Canary: sample(200, 1, ms(80)), Baseline: healthy}, Promote, ""},
		{"5xx over ceiling fails fast", Input{Elapsed: time.Minute, Canary: sample(100, 6, ms(80)), Baseline: healthy}, Rollback, "5xx oranı %6.0"},
		{"5xx worse than baseline", Input{Elapsed: step, Canary: sample(100, 4, ms(80)), Baseline: sample(1000, 10, ms(80))}, Rollback, "puan"},
		{"5xx slightly worse is fine", Input{Elapsed: step, Canary: sample(100, 2, ms(80)), Baseline: sample(1000, 10, ms(80))}, Advance, ""},
		{"baseline below min requests: no relative check", Input{Elapsed: step, Canary: sample(100, 4, nil), Baseline: sample(10, 0, nil)}, Advance, ""},
		{"p95 factor", Input{Elapsed: time.Minute, Canary: sample(100, 0, ms(300)), Baseline: sample(1000, 0, ms(100))}, Rollback, "kat"},
		{"p95 factor under floor", Input{Elapsed: step, Canary: sample(100, 0, ms(12)), Baseline: sample(1000, 0, ms(4))}, Advance, ""},
		{"p95 absolute", Input{Elapsed: step, Settings: abs, Canary: sample(100, 0, ms(350)), Baseline: sample(1000, 0, ms(340))}, Rollback, "350 ms, eşik 300"},
		{"no histogram: rates only", Input{Elapsed: step, Settings: abs, Canary: sample(100, 0, nil), Baseline: healthy}, Advance, "p95 yok"},
		{"low traffic, no evidence: advance on time", Input{Elapsed: step, Canary: sample(7, 1, nil), Baseline: sample(30, 0, nil)}, Advance, "yetersiz trafik (7 < 50"},
		{"low traffic, still running", Input{Elapsed: time.Minute, Canary: sample(7, 1, nil)}, Hold, ""},
		{"low traffic, clear evidence", Input{Elapsed: time.Minute, Canary: sample(8, 6, nil)}, Rollback, "hata kanıtı"},
		{"low traffic, four errors are not enough", Input{Elapsed: step, Canary: sample(4, 4, nil)}, Advance, ""},
		{"idle app advances", Input{Elapsed: step}, Advance, "yetersiz trafik"},
		{"metrics missing: hold", Input{Elapsed: step, ExpectTraffic: true}, Hold, "metriği yok"},
		{"metrics missing too long: abort", Input{Elapsed: 3 * step, ExpectTraffic: true}, Abort, "alınamadı"},
		{"guard healthy", Input{Mode: store.RolloutGuarded, Elapsed: 10 * time.Minute, Duration: 10 * time.Minute, LastStep: true,
			Canary: sample(500, 2, ms(90)), Baseline: healthy}, Promote, ""},
		{"guard: nothing measured since the switch", Input{Mode: store.RolloutGuarded, Elapsed: 10 * time.Minute, Duration: 10 * time.Minute,
			LastStep: true, ExpectTraffic: true, Baseline: healthy}, Hold, "metriği yok"},
		{"canary: stable has traffic, canary none yet", Input{Elapsed: step, ExpectTraffic: true, Baseline: healthy}, Advance, "yetersiz"},
		{"guard regression", Input{Mode: store.RolloutGuarded, Elapsed: 2 * time.Minute, Duration: 10 * time.Minute, LastStep: true,
			Canary: sample(500, 100, ms(90)), Baseline: healthy}, Rollback, "%20.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			if in.Mode == "" {
				in.Mode = store.RolloutCanary
			}
			if in.Duration == 0 {
				in.Duration = step
			}
			if in.Settings.Mode == "" {
				in.Settings = def
			}
			got := Decide(in)
			if got.Action != c.want || !strings.Contains(got.Reason, c.contains) {
				t.Fatalf("got %s %q, want %s containing %q", got.Action, got.Reason, c.want, c.contains)
			}
		})
	}
}

func TestMinutesBetween(t *testing.T) {
	at := func(m, s int) time.Time { return time.Date(2026, 10, 5, 12, m, s, 0, time.UTC) }
	w := minutesBetween(at(0, 20), at(5, 40))
	if !w.from.Equal(at(1, 0)) || !w.to.Equal(at(5, 0)) {
		t.Fatalf("window %v", w)
	}
	w = minutesBetween(at(3, 0), at(3, 30))
	if !w.from.Equal(at(3, 0)) || !w.to.Equal(at(3, 0)) {
		t.Fatalf("empty window %v", w)
	}
}
