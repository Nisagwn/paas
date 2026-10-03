package worker

import (
	"context"
	"time"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// DryRunPipeline walks through the pipeline stages without building or
// deploying anything. It lets the control plane (webhook → queue → statuses →
// aliases → rollback) be developed and demoed before Faz 2 and 3 exist.
type DryRunPipeline struct {
	// Registry prefix for the image name it pretends to produce.
	Registry string
	// Step is the simulated duration of each stage.
	Step time.Duration
}

func (p DryRunPipeline) Build(ctx context.Context, d store.Deployment, log Logger) (string, error) {
	image := p.Registry + "/" + d.AppName + ":" + d.CommitSHA
	log("[dry-run] git clone --depth 1 %s (commit %s)", d.AppName, naming.ShortSHA(d.CommitSHA))
	if err := sleep(ctx, p.Step); err != nil {
		return "", err
	}
	log("[dry-run] buildkit build → %s", image)
	if err := sleep(ctx, p.Step); err != nil {
		return "", err
	}
	return image, nil
}

func (p DryRunPipeline) Deploy(ctx context.Context, d store.Deployment, image string, log Logger) error {
	log("[dry-run] apply Deployment+Service %s/%s", naming.Namespace(d.AppName), naming.ResourceName(d.CommitSHA))
	if err := sleep(ctx, p.Step); err != nil {
		return err
	}
	log("[dry-run] readiness probe passed")
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
