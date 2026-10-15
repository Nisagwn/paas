package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func imgDigest(n int) string { return fmt.Sprintf("sha256:%064x", n) }

// Faz 30: an image leaves the registry only when no kept deployment
// references its manifest, decided per digest (image reuse and identical
// builds share one), and a redeploy never reuses a released image.
func TestReleaseImage(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	other := mustApp(t, st, "shop")
	const repo = "localhost:5000/blog"
	a, b := imgDigest(1), imgDigest(2)

	built := func(appID int64, n int, image string) store.Deployment {
		t.Helper()
		if _, _, err := st.EnqueueDeployment(ctx, appID, sha(n), "feature", ""); err != nil {
			t.Fatal(err)
		}
		d, err := st.ClaimNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetImage(ctx, d.ID, image); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkReady(ctx, d, nil); err != nil {
			t.Fatal(err)
		}
		d, _ = st.GetDeployment(ctx, d.ID)
		return d
	}
	retire := func(d store.Deployment) {
		t.Helper()
		if ok, err := st.Retire(ctx, d.ID, "test"); err != nil || !ok {
			t.Fatalf("retire %d: %v %v", d.ID, ok, err)
		}
	}
	release := func(d store.Deployment, digest string) bool {
		t.Helper()
		ok, err := st.ReleaseImage(ctx, d.ID, repo, digest, d.Image)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	released := func(d store.Deployment) bool {
		t.Helper()
		r, err := st.ImageReleased(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	d1 := built(app.ID, 1, repo+":"+sha(1)+"-1@"+a)
	// d3: another build of another commit that produced the same digest.
	d3 := built(app.ID, 3, repo+":"+sha(3)+"-3@"+a)
	d4 := built(app.ID, 4, repo+":"+sha(4)+"-4@"+b)
	// The same digest in another repository is another manifest.
	dOther := built(other.ID, 5, "localhost:5000/shop:"+sha(5)+"-5@"+a)
	// d2 reuses d1's image (a redeploy with the image); it stays queued.
	d2, err := st.CopyDeployment(ctx, d1, store.CopyOptions{Origin: store.OriginRedeploy, Target: store.EnvPreview, ReuseImage: true})
	if err != nil || d2.Image != d1.Image {
		t.Fatalf("copy: %+v %v", d2, err)
	}

	// A kept deployment's own image is never released.
	if release(d4, b) {
		t.Fatal("released the image of a ready deployment")
	}
	retire(d1)
	if release(d1, a) {
		t.Fatal("released a digest that queued d2 and ready d3 still reference")
	}
	claimed, _ := st.ClaimNext(ctx)
	if claimed.ID != d2.ID {
		t.Fatalf("claimed %d, want %d", claimed.ID, d2.ID)
	}
	st.MarkFailed(ctx, d2.ID, "boom")
	if release(d1, a) {
		t.Fatal("ready d3 still references the digest")
	}
	retire(d3)
	if !release(d1, a) {
		t.Fatal("nothing kept references the digest any more")
	}
	for _, d := range []store.Deployment{d1, d2, d3} {
		if !released(d) {
			t.Errorf("deployment %d shares the deleted manifest but is not marked released", d.ID)
		}
	}
	if released(d4) || released(dOther) {
		t.Error("other manifests were marked released")
	}
	// Repeating (a failed registry delete is retried) is safe.
	if !release(d3, a) {
		t.Fatal("repeated release")
	}

	// A redeploy of a released deployment builds again; of a kept one, reuses.
	r1, err := st.CopyDeployment(ctx, d1, store.CopyOptions{Origin: store.OriginRedeploy, Target: store.EnvPreview, ReuseImage: true})
	if err != nil || r1.Image != "" {
		t.Fatalf("copy of a released image: %+v %v", r1, err)
	}
	r4, err := st.CopyDeployment(ctx, d4, store.CopyOptions{Origin: store.OriginRedeploy, Target: store.EnvPreview, ReuseImage: true})
	if err != nil || r4.Image != d4.Image {
		t.Fatalf("copy of a kept image: %+v %v", r4, err)
	}

	// An image without a recorded digest is matched by its reference.
	tagOnly := built(app.ID, 6, repo+":"+sha(6)+"-6")
	retire(tagOnly)
	if !release(tagOnly, imgDigest(6)) || !released(tagOnly) {
		t.Fatal("tag-only image")
	}

	// A revived deployment (its commit pushed again) starts without image.
	if err := st.MarkCleaned(ctx, d1.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := st.EnqueueDeployment(ctx, app.ID, sha(1), "feature", ""); err != nil || !created {
		t.Fatalf("revive: %v %v", created, err)
	}
	if released(d1) {
		t.Error("revive keeps image_released_at")
	}

	if _, err := st.ReleaseImage(ctx, d1.ID, "", "", ""); err == nil {
		t.Error("an empty reference is invalid")
	}
}
