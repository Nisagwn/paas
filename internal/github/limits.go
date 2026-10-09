package github

import (
	"context"
	"errors"
	"strings"
)

// DeploymentRefused marks a pushed commit that was not deployed because of
// the team's plan (Faz 23, api.RefusalReporter): a failed commit status
// "Plan limiti aşıldı" linking to the usage page.
func (n *Notifier) DeploymentRefused(ctx context.Context, repo, sha, reason string) error {
	target := ""
	if n.PublicURL != "" {
		target = strings.TrimRight(n.PublicURL, "/") + "/usage"
	}
	err := n.Client.CreateCommitStatus(ctx, repo, sha, Status{
		State:       StateFailure,
		TargetURL:   target,
		Description: Truncate("Plan limiti aşıldı (limit reached): "+oneLine(reason), 140),
		Context:     n.context(),
	})
	if errors.Is(err, ErrNoCredentials) {
		return nil
	}
	return err
}
