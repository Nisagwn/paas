package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/appdelete"
	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

// doomed (re)creates app "doomed" in team web for the authorization
// matrix: every successful DELETE removes it.
func (f *teamFixture) doomed() {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.st.GetAppByName(ctx, "doomed"); err == nil {
		return
	}
	if _, err := f.st.CreateAppInTeam(ctx, f.web.ID, "doomed", "nisagwn/doomed", "main"); err != nil {
		f.t.Fatal(err)
	}
}

// blockingBuild builds until its context is canceled and the test
// releases it, so the deployment is still building when DELETE runs.
type blockingBuild struct{ started, release chan int64 }

func (p blockingBuild) Build(ctx context.Context, d store.Deployment, _ store.BuildSettings, _ worker.Logger) (worker.BuildResult, error) {
	p.started <- d.ID
	<-ctx.Done()
	<-p.release
	return worker.BuildResult{}, ctx.Err()
}

func (blockingBuild) Deploy(context.Context, store.Deployment, string, worker.Logger) error {
	return nil
}

// recordingCluster counts DeleteApp calls; the namespace is gone at once.
type recordingCluster struct{ apps []string }

func (c *recordingCluster) DeleteApp(_ context.Context, app string, _ int64) (bool, error) {
	c.apps = append(c.apps, app)
	return true, nil
}

// Faz 28: DELETE /api/apps/{name} checks the confirmation, stops the
// running deployment through the cancel machinery, answers 202 while it
// stops, and the background step finishes the deletion: the app is gone,
// pushes are ignored, the name and repo can be used again.
func TestDeleteAppAPI(t *testing.T) {
	f := setupTeams(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cluster := &recordingCluster{}
	deleter := &appdelete.Service{Store: f.st, Cluster: cluster, Log: log,
		Notify: &notify.Service{Store: f.st, Log: log}}
	srv := httptest.NewServer((&api.Server{
		Store: f.st, Domain: domain, APIToken: token, WebhookSecret: secret, Log: log, Sessions: f.ss, Deleter: deleter,
	}).Handler())
	t.Cleanup(srv.Close)
	f.srv = srv
	e := &env{t: t, srv: srv}

	ch := f.notificationChannel("ops")
	// A build of blog is running.
	blog, _ := f.st.GetAppByName(ctx, "blog")
	running, _, _ := f.st.EnqueueDeployment(ctx, blog.ID, sha(7), "feature", "running")
	p := blockingBuild{started: make(chan int64, 1), release: make(chan int64)}
	w := &worker.Worker{Store: f.st, Pipeline: p, Domain: domain, Timeout: 10 * time.Second,
		CancelPoll: 20 * time.Millisecond, Log: log}
	finished := make(chan error, 1)
	go func() {
		_, err := w.ProcessOne(ctx)
		finished <- err
	}()
	select {
	case id := <-p.started:
		if id != running.ID {
			t.Fatalf("worker claimed %d, want %d", id, running.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("build did not start")
	}
	queued, _, _ := f.st.EnqueueDeployment(ctx, blog.ID, sha(8), "feature", "queued")

	// The confirmation must be the name.
	if code, body := f.call("alice", "DELETE", "/api/apps/blog", map[string]string{"confirm": "blgo"}); code != 400 ||
		!strings.Contains(body, `\"blog\"`) && !strings.Contains(body, `("blog")`) {
		t.Fatalf("mismatch: %d %s", code, body)
	}
	if code, _ := f.call("alice", "DELETE", "/api/apps/blog", nil); code != 400 {
		t.Fatalf("no body: %d", code)
	}
	if code, _ := f.call("bob", "DELETE", "/api/apps/blog", map[string]string{"confirm": "blog"}); code != 403 {
		t.Fatalf("member: %d", code)
	}
	if _, err := f.st.GetAppByName(ctx, "blog"); err != nil {
		t.Fatalf("refused deletions changed the app: %v", err)
	}

	code, body := f.call("alice", "DELETE", "/api/apps/blog", map[string]string{"confirm": "blog"})
	if code != 202 || !strings.Contains(body, `"status":"deleting"`) {
		t.Fatalf("delete with a running build: %d %s", code, body)
	}
	// The app is gone at once for the API and for pushes.
	if code, _ := f.call("alice", "GET", "/api/apps/blog", nil); code != 404 {
		t.Fatalf("GET after delete: %d", code)
	}
	if code, _ := f.call("alice", "DELETE", "/api/apps/blog", map[string]string{"confirm": "blog"}); code != 404 {
		t.Fatalf("second DELETE: %d", code)
	}
	if code, out := e.push("nisagwn/blog", "main", sha(9)); code != 202 || !strings.Contains(out["reason"].(string), "no app") {
		t.Fatalf("push after delete: %d %v", code, out)
	}
	if code, body := f.call("alice", "POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog", "team": "web"}); code != 409 ||
		!strings.Contains(body, "still being deleted") {
		t.Fatalf("create while deleting: %d %s", code, body)
	}
	if d, _ := f.st.GetDeployment(ctx, queued.ID); d.Status != store.StatusCanceled {
		t.Fatalf("queued deployment: %+v", d)
	}
	// Still building: the background step waits.
	deleter.Sweep(ctx)
	if len(cluster.apps) != 0 {
		t.Fatalf("cluster cleaned while a build ran: %v", cluster.apps)
	}
	// The worker's heartbeat sees the cancel request and stops the build.
	close(p.release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("running build was not canceled")
	}
	if d, _ := f.st.GetDeployment(ctx, running.ID); d.Status != store.StatusCanceled {
		t.Fatalf("running deployment: %+v", d)
	}
	if len(cluster.apps) != 0 {
		t.Fatalf("cluster cleaned while a build ran: %v", cluster.apps)
	}

	// The background step finishes it.
	deleter.Sweep(ctx)
	if len(cluster.apps) != 1 || cluster.apps[0] != "blog" {
		t.Fatalf("cluster: %v", cluster.apps)
	}
	if _, err := f.st.GetAppByID(ctx, blog.ID); err == nil {
		t.Fatal("app row survived")
	}
	if _, err := f.st.GetDeployment(ctx, running.ID); err == nil {
		t.Fatal("deployment row survived")
	}
	if pending, _ := f.st.PendingAppDeletions(ctx); len(pending) != 0 {
		t.Fatalf("pending: %+v", pending)
	}
	// The team's channel got the project.deleted event, with the name.
	deliveries, err := f.st.ListNotificationDeliveries(ctx, ch.ID, 10)
	if err != nil || len(deliveries) != 1 || deliveries[0].EventType != notify.ProjectDeleted ||
		!strings.Contains(string(deliveries[0].Payload), `"deleted_by": "alice"`) {
		t.Fatalf("deliveries: %+v %v", deliveries, err)
	}
	// Name and repo are free again.
	if code, body := f.call("alice", "POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog", "team": "web"}); code != 201 {
		t.Fatalf("recreate: %d %s", code, body)
	}
	if code, _ := e.push("nisagwn/blog", "main", sha(9)); code != 201 {
		t.Fatalf("push to the recreated app: %d", code)
	}

	// Nothing running: the deletion is done in the request (204).
	if code, body := f.call("admin", "DELETE", "/api/apps/blog", map[string]string{"confirm": "blog"}); code != http.StatusNoContent || body != "" {
		t.Fatalf("admin delete: %d %q", code, body)
	}
	if code, _ := f.call("alice", "GET", "/api/apps/blog", nil); code != 404 {
		t.Fatalf("GET after the second delete: %d", code)
	}
	if code, _ := f.call("dave", "DELETE", "/api/apps/infra", map[string]string{"confirm": "infra"}); code != 204 {
		t.Fatalf("dave deletes his own app: %d", code)
	}
}
