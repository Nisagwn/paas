package rollout

import (
	"fmt"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

// Action is what the controller does with a rollout after one evaluation.
type Action string

const (
	Hold     Action = "hold"     // keep the current weight and evaluate again later
	Advance  Action = "advance"  // next canary weight, new step window
	Promote  Action = "promote"  // canary: the production alias moves; guard: the watch ends well
	Rollback Action = "rollback" // canary: weight 0; guard: production moves back
	Abort    Action = "abort"    // no verdict possible (metrics missing): end, production unchanged
)

// Tunables of the decision that are not settings.
const (
	// MinErrorEvidence: below MinRequests, a canary is rolled back only if
	// at least this many of its responses were 5xx (and their rate is over
	// the ceiling): two failed requests out of three are noise, five are a
	// pattern.
	MinErrorEvidence = 5
	// LatencyFloorMs: a p95 above factor × the baseline's only counts when
	// it is also this much slower in absolute terms (2 ms vs 5 ms is not a
	// regression worth a rollback).
	LatencyFloorMs = 25
	// MissingMetricsSteps: with no metrics on either side for this many
	// step durations while production used to have traffic, the rollout is
	// aborted (production unchanged) instead of advancing blind.
	MissingMetricsSteps = 3
)

// Input is everything one decision depends on.
type Input struct {
	Mode string // store.RolloutCanary or store.RolloutGuarded
	// Elapsed time of the current step (canary) or since the switch
	// (guard); Duration is its length.
	Elapsed, Duration time.Duration
	// LastStep: advancing promotes (always true for a guard).
	LastStep bool
	// Canary is the new deployment's traffic in the window, Baseline the
	// current (canary) or previous (guard) production's.
	Canary, Baseline store.RolloutSample
	// ExpectTraffic: production served at least MinRequests in the
	// reference window before the rollout, so zero requests on both sides
	// now means the metrics are missing rather than the app being idle.
	ExpectTraffic bool
	Settings      store.RolloutSettings
}

// Verdict is the decision and its reason (Turkish: it is shown in the UI,
// the deployment log and the GitHub commit status).
type Verdict struct {
	Action Action
	Reason string
}

// Decide is the rollout policy, a pure function of in:
//
//  1. Failure, at any time of a step (fail fast):
//     with at least MinRequests canary requests, the canary is rolled back
//     when its 5xx rate exceeds MaxErrorPct, or exceeds the baseline's rate
//     by more than MaxErrorIncreasePct points (baseline with MinRequests),
//     or its p95 exceeds MaxP95Ms, or exceeds MaxP95Factor × the
//     baseline's p95 (and by LatencyFloorMs);
//     with fewer requests, only clear evidence counts: at least
//     MinErrorEvidence 5xx responses and a rate above MaxErrorPct.
//  2. The step is still running: hold.
//  3. Metrics missing (no requests on either side, for a guard none on the
//     new production, although production used to have traffic): hold,
//     and abort after MissingMetricsSteps.
//  4. Otherwise the step passed: advance, or promote at the last step.
//     A low-traffic app (fewer than MinRequests) advances on time when
//     there is no evidence of failure: waiting would never produce one.
func Decide(in Input) Verdict {
	s := in.Settings
	c, b := in.Canary, in.Baseline
	min := int64(s.MinRequests)
	if c.Requests >= min {
		if rate := c.ErrorPct(); rate > s.MaxErrorPct {
			return Verdict{Rollback, fmt.Sprintf("5xx oranı %%%.1f, eşik %%%.1f (%d/%d istek)", rate, s.MaxErrorPct, c.Errors, c.Requests)}
		}
		if b.Requests >= min && c.ErrorPct()-b.ErrorPct() > s.MaxErrorIncreasePct {
			return Verdict{Rollback, fmt.Sprintf("5xx oranı %%%.1f, production %%%.1f: fark %.1f puan > %.1f puan",
				c.ErrorPct(), b.ErrorPct(), c.ErrorPct()-b.ErrorPct(), s.MaxErrorIncreasePct)}
		}
		if c.P95Ms != nil {
			p95 := *c.P95Ms
			if s.MaxP95Ms > 0 && p95 > float64(s.MaxP95Ms) {
				return Verdict{Rollback, fmt.Sprintf("p95 gecikme %.0f ms, eşik %d ms", p95, s.MaxP95Ms)}
			}
			if s.MaxP95Factor > 0 && b.P95Ms != nil && b.Requests >= min {
				base := *b.P95Ms
				if p95 > s.MaxP95Factor*base && p95-base >= LatencyFloorMs {
					return Verdict{Rollback, fmt.Sprintf("p95 gecikme %.0f ms, production %.0f ms: %.1f kat > %.1f kat",
						p95, base, p95/max(base, 0.1), s.MaxP95Factor)}
				}
			}
		}
	} else if c.Errors >= MinErrorEvidence && c.ErrorPct() > s.MaxErrorPct {
		return Verdict{Rollback, fmt.Sprintf("az trafik ama hata kanıtı var: %d/%d istek 5xx (%%%.1f)", c.Errors, c.Requests, c.ErrorPct())}
	}

	if in.Elapsed < in.Duration {
		return Verdict{Hold, fmt.Sprintf("adım sürüyor (%s / %s)", in.Elapsed.Truncate(time.Second), in.Duration)}
	}
	// A guard's baseline is the traffic before the switch; after it the new
	// deployment serves everything, so its zero alone means missing metrics.
	if c.Requests == 0 && in.ExpectTraffic && (b.Requests == 0 || in.Mode == store.RolloutGuarded) {
		if in.Elapsed >= MissingMetricsSteps*in.Duration {
			return Verdict{Abort, fmt.Sprintf("istek metrikleri %s boyunca alınamadı; production değişmedi", in.Elapsed.Truncate(time.Second))}
		}
		return Verdict{Hold, "iki tarafta da istek metriği yok (Traefik metrikleri alınamıyor olabilir); bekleniyor"}
	}

	next := Advance
	if in.LastStep {
		next = Promote
	}
	if c.Requests < min {
		return Verdict{next, fmt.Sprintf("yetersiz trafik (%d < %d istek): hata kanıtı yok, süre doldu", c.Requests, min)}
	}
	return Verdict{next, fmt.Sprintf("eşikler içinde: 5xx %%%.1f, %s", c.ErrorPct(), p95Text(c.P95Ms))}
}

func p95Text(p *float64) string {
	if p == nil {
		return "p95 yok"
	}
	return fmt.Sprintf("p95 %.0f ms", *p)
}
