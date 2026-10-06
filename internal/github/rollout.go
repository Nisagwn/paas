package github

import (
	"context"
	"fmt"

	"github.com/nisagwn/paas/internal/store"
)

// Faz 21: the outcome of a canary or guarded rollout is reported as a
// second commit status next to the deploy status ("paas/deploy/rollout"),
// so the deploy's own green check stays: the deployment did build and run;
// the rollout status says whether it took production.

// RolloutContext is the status context of rollouts for the deploy context c.
func RolloutContext(c string) string { return c + "/rollout" }

// RolloutFinished sets the rollout's commit status on the deployment's
// commit (rollout.Notifier).
func (n *Notifier) RolloutFinished(ctx context.Context, d store.Deployment, r store.Rollout) error {
	s := Status{Context: RolloutContext(n.context()), TargetURL: n.DeploymentPage(d.ID)}
	kind := "Canary"
	if r.Mode == store.RolloutGuarded {
		kind = "İzleme"
	}
	switch r.State {
	case store.RolloutPromoted:
		s.State = StateSuccess
		s.Description = kind + " tamamlandı: production bu deploy"
	case store.RolloutRolledBack:
		s.State = StateFailure
		s.Description = kind + " geri alındı: " + oneLine(r.Reason)
	case store.RolloutAborted, store.RolloutSuperseded:
		s.State = StateError
		s.Description = fmt.Sprintf("%s durduruldu: %s", kind, oneLine(r.Reason))
	default:
		return nil
	}
	return n.skipUncredentialed(d, n.Client.CreateCommitStatus(ctx, d.Repo, d.CommitSHA, s))
}
