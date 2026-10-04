package github_test

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/nisagwn/paas/internal/build"
	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/worker"
)

// A deployment of a repository the App is installed on clones and reports
// statuses with that installation's token, through the real worker and
// builder.
func TestWorkerUsesInstallationToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	st := testdb.Open(t)
	ctx := context.Background()
	f := newFakeApp(t, st)

	// The Git host: records how the clone authenticated, has no repository.
	var mu sync.Mutex
	var cloneAuth []string
	gitHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cloneAuth = append(cloneAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(gitHost.Close)

	if err := st.UpsertInstallation(ctx, store.Installation{ID: 7, AccountLogin: "acme", AccountType: "Organization"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetInstallationRepos(ctx, 7, []store.InstallationRepo{{RepoID: 1, FullName: "acme/web"}}); err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateApp(ctx, "web", "acme/web", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnqueueDeployment(ctx, app.ID, sha, "main", ""); err != nil {
		t.Fatal(err)
	}

	tokens := github.AppTokens{App: f.app, Fallback: "pat"}
	client := github.New(f.srv.URL, "pat", nil)
	client.Tokens = tokens
	w := &worker.Worker{
		Store: st, Domain: "paas.test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pipeline: worker.Stages{
			Builder: &build.Builder{Engine: build.Docker{}, Registry: "r", GitBaseURL: gitHost.URL,
				GitToken: "pat", GitTokens: tokens, WorkDir: t.TempDir()},
			Deployer: worker.DryRunPipeline{},
		},
		Notifier: &github.Notifier{Client: client},
	}
	if _, err := w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:inst-7-1"))
	mu.Lock()
	defer mu.Unlock()
	if len(cloneAuth) == 0 || cloneAuth[0] != want {
		t.Fatalf("clone Authorization = %q, want %q", cloneAuth, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.statusAuth) != 2 || strings.Join(f.statusAuth, ",") != "Bearer inst-7-1,Bearer inst-7-1" {
		t.Fatalf("status Authorization = %q", f.statusAuth)
	}
	if f.mints[7] != 1 {
		t.Fatalf("%d installation tokens minted for one deployment", f.mints[7])
	}
}
