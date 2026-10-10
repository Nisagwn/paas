package github_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/github"
)

// A push refused by the team's plan gets a failed commit status that
// links to the usage page.
func TestDeploymentRefused(t *testing.T) {
	f := newFake(t)
	n := &github.Notifier{Client: f.c, PublicURL: "https://paas.test/"}
	if err := n.DeploymentRefused(context.Background(), "nisagwn/blog", sha,
		"plan limit reached: the Hobby plan allows 50 deployments per day"); err != nil {
		t.Fatal(err)
	}
	if len(f.statuses) != 1 {
		t.Fatalf("statuses %+v", f.statuses)
	}
	s := f.statuses[0]
	if s.State != "failure" || s.TargetURL != "https://paas.test/usage" || s.Context != "paas/deploy" ||
		!strings.HasPrefix(s.Description, "Plan limiti aşıldı (limit reached): plan limit reached") {
		t.Fatalf("status %+v", s)
	}
}
