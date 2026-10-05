package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func TestBuildSettingsRoundTrip(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, "web", "nisagwn/web", "main")
	other, _ := st.CreateApp(ctx, "api", "nisagwn/api", "main")

	// Never saved: everything detected.
	got, err := st.GetBuildSettings(ctx, app.ID)
	if err != nil || got != (store.BuildSettings{}) {
		t.Fatalf("default = %+v, %v", got, err)
	}

	want := store.BuildSettings{
		RootDirectory: "apps/web", Framework: "nextjs", InstallCommand: "pnpm install",
		BuildCommand: "pnpm build", StartCommand: "node server.js", OutputDirectory: "out", NodeVersion: "20",
	}
	saved, err := st.UpdateBuildSettings(ctx, app.ID, want)
	if err != nil || saved.UpdatedAt == nil {
		t.Fatalf("update = %+v, %v", saved, err)
	}
	got, err = st.GetBuildSettings(ctx, app.ID)
	if err != nil || got.UpdatedAt == nil {
		t.Fatalf("get = %+v, %v", got, err)
	}
	got.UpdatedAt = nil
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}

	// A second update replaces the row; other apps are untouched.
	if _, err := st.UpdateBuildSettings(ctx, app.ID, store.BuildSettings{Framework: "vite"}); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetBuildSettings(ctx, app.ID); got.Framework != "vite" || got.RootDirectory != "" {
		t.Fatalf("after replace = %+v", got)
	}
	if got, _ = st.GetBuildSettings(ctx, other.ID); got != (store.BuildSettings{}) {
		t.Fatalf("other app = %+v", got)
	}
	if _, err := st.UpdateBuildSettings(ctx, 999999, want); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown app: %v", err)
	}

	// Deployments record the framework their build used.
	if f, err := st.DetectedFramework(ctx, app.ID); err != nil || f != "" {
		t.Fatalf("no deployments: %q %v", f, err)
	}
	d1, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "")
	d2, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(2), "main", "")
	st.SetFramework(ctx, d1.ID, "Vite")
	if f, _ := st.DetectedFramework(ctx, app.ID); f != "Vite" {
		t.Fatalf("detected = %q", f)
	}
	st.SetFramework(ctx, d2.ID, "Next.js")
	if d, _ := st.GetDeployment(ctx, d2.ID); d.Framework != "Next.js" {
		t.Fatalf("deployment framework = %q", d.Framework)
	}
	if f, _ := st.DetectedFramework(ctx, app.ID); f != "Next.js" {
		t.Fatalf("detected = %q", f)
	}
}
