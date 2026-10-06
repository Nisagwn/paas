package worker

import (
	"fmt"
	"strings"

	"github.com/nisagwn/paas/internal/store"
)

// Faz 21: a production deployment of an app in canary or guarded mode.

// rolloutAliases logs the rollout MarkReadyWithRollout started and returns
// the aliases that actually moved (a canary leaves production alone).
func rolloutAliases(aliases []store.AliasSpec, ro *store.Rollout, log Logger) []store.AliasSpec {
	if ro == nil {
		return aliases
	}
	from := "?"
	if ro.FromDeploymentID != nil {
		from = fmt.Sprintf("#%d", *ro.FromDeploymentID)
	}
	if ro.Mode == store.RolloutGuarded {
		log("==> izlemeli geçiş: production %s deploy'undan buna geçti; %s izleniyor (5xx/gecikme bozulursa otomatik geri alınır)",
			from, ro.Settings.GuardDuration())
		return aliases
	}
	steps := make([]string, len(ro.Settings.Steps))
	for i, w := range ro.Settings.Steps {
		steps[i] = fmt.Sprintf("%%%d", w)
	}
	log("==> canary: %%%d trafik (production şimdilik %s deploy'unda; adımlar %s, her biri %s)",
		ro.Weight, from, strings.Join(steps, " → "), ro.Settings.StepDuration())
	out := make([]store.AliasSpec, 0, len(aliases))
	for _, a := range aliases {
		if a.Kind != store.AliasProduction {
			out = append(out, a)
		}
	}
	return out
}
