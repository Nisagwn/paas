package cleanup_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/cleanup"
	"github.com/nisagwn/paas/internal/registry"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

// fakeImages is a registry: "repo:tag" → digest.
type fakeImages struct {
	mu          sync.Mutex
	host        string
	tags        map[string]string
	deleted     []string // "repo@digest"
	fail        error
	unsupported bool
}

func (f *fakeImages) Owns(img registry.Image) bool { return img.Host == f.host }

func (f *fakeImages) Resolve(_ context.Context, img registry.Image) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if img.Digest != "" {
		return img.Digest, nil
	}
	return f.tags[img.Repository+":"+img.Tag], nil
}

func (f *fakeImages) Delete(_ context.Context, repo, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if f.unsupported {
		return fmt.Errorf("delete: %w", registry.ErrUnsupported)
	}
	f.deleted = append(f.deleted, repo+"@"+digest)
	return nil
}

func (f *fakeImages) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.deleted...)
	sort.Strings(out)
	return out
}

func digest(n int) string { return fmt.Sprintf("sha256:%064x", n) }

// Faz 30: the registry keeps exactly the images of kept deployments. A
// retired or failed deployment's image goes after its cluster objects,
// unless a kept deployment runs the same manifest; a failed registry delete
// keeps the deployment pending and is retried.
func TestCollectImages(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	prod := store.AliasSpec{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}
	const repo = "localhost:5000/blog"

	ready := func(n int, image string) store.Deployment {
		t.Helper()
		d, _, err := st.EnqueueDeployment(ctx, app.ID, sha(n), "main", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetImage(ctx, d.ID, image); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkReady(ctx, d, []store.AliasSpec{prod}); err != nil {
			t.Fatal(err)
		}
		return d
	}
	// m1: its own image. m2 and m4 run one manifest (m4 redeployed m2's
	// image). m3: an image on another registry. Production points at m5;
	// m4 and m5 are the two kept production deployments.
	m1 := ready(1, repo+":"+sha(1)+"-1@"+digest(1))
	m2 := ready(2, repo+":"+sha(2)+"-2@"+digest(2))
	ready(3, "ghcr.io/x/blog:"+sha(3)+"@"+digest(3))
	ready(4, repo+":"+sha(2)+"-2@"+digest(2))
	m5 := ready(5, repo+":"+sha(5)+"-5@"+digest(5))
	// A deployment that failed after its push; no digest was recorded.
	failed, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(6), "feature", "")
	st.SetImage(ctx, failed.ID, repo+":"+sha(6)+"-6")
	st.MarkFailed(ctx, failed.ID, "CrashLoopBackOff")

	images := &fakeImages{host: "localhost:5000", tags: map[string]string{"blog:" + sha(6) + "-6": digest(6)},
		fail: errors.New("registry unavailable")}
	c := cleanup.New(st, &fakeRetirer{}, nil, cleanup.Policy{KeepProduction: 2}, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.Images = images

	// The registry is down: m1 and the failed deployment stay pending; m2
	// (its manifest is kept for m4) and m3 (foreign registry) are done.
	r := c.CollectApp(ctx, app, false)
	if r.Retired != 3 || r.Cleaned != 2 || r.Errors != 2 {
		t.Fatalf("first sweep %+v, want 3 retired, 2 cleaned, 2 errors", r)
	}
	if pending, _ := st.PendingCleanup(ctx, app.ID, 10); len(pending) != 2 {
		t.Fatalf("pending after a registry failure: %d", len(pending))
	}

	images.fail = nil
	r = c.CollectApp(ctx, app, false)
	if r.Cleaned != 2 || r.Errors != 0 {
		t.Fatalf("second sweep %+v", r)
	}
	want := []string{"blog@" + digest(1), "blog@" + digest(6)}
	if got := images.list(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("deleted %v, want %v (m2's manifest runs in kept m4)", got, want)
	}
	if rel, _ := st.ImageReleased(ctx, m2.ID); rel {
		t.Error("m2's image is still used by m4")
	}
	if rel, _ := st.ImageReleased(ctx, m1.ID); !rel {
		t.Error("m1's image was deleted but is not marked released")
	}
	logs, _ := st.Logs(ctx, m1.ID, 0, 100)
	if !strings.Contains(fmt.Sprint(logs), "image deleted from the registry: "+repo+"@"+digest(1)) {
		t.Errorf("m1's log lacks the image deletion: %v", logs)
	}
	if d, _ := st.GetDeployment(ctx, m5.ID); d.Status != store.StatusReady {
		t.Errorf("live deployment is %s", d.Status)
	}

	// m7 and m8 push m4 and m5 out. The registry refuses deletes: the
	// images stay and the deployments are cleaned without errors.
	images.unsupported = true
	ready(7, repo+":"+sha(7)+"-7@"+digest(7))
	ready(8, repo+":"+sha(8)+"-8@"+digest(8))
	r = c.CollectApp(ctx, app, false)
	if r.Retired != 2 || r.Cleaned != 2 || r.Errors != 0 {
		t.Fatalf("delete-disabled sweep %+v", r)
	}
}
