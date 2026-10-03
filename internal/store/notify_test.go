package store_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func startHub(t *testing.T) *store.Hub {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hub := store.NewHub(os.Getenv("PAAS_TEST_DATABASE_URL"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { done <- hub.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("hub.Run: %v", err)
		}
	})
	select {
	case <-hub.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("hub did not start listening")
	}
	return hub
}

func waitWake(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatalf("no wake-up after %s", what)
	}
}

func drain(c <-chan struct{}) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
}

// Log inserts and status changes wake only the subscribers of that
// deployment; a dropped listen connection wakes everyone once it is back.
func TestHubNotifications(t *testing.T) {
	st := testdb.Open(t)
	hub := startHub(t)
	ctx := context.Background()

	app, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	d1, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "")
	d2, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(2), "main", "")

	c1, cancel1 := hub.Subscribe(d1.ID)
	defer cancel1()
	c2, cancel2 := hub.Subscribe(d2.ID)
	defer cancel2()
	drain(c1) // the start-up wake-up
	drain(c2)

	if err := st.AppendLog(ctx, d1.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	waitWake(t, c1, "AppendLog")

	if err := st.SetStatus(ctx, d1.ID, store.StatusDeploying); err != nil {
		t.Fatal(err)
	}
	waitWake(t, c1, "SetStatus")
	select {
	case <-c2:
		t.Fatal("subscriber of another deployment was woken")
	case <-time.After(300 * time.Millisecond):
	}

	// Setting the same status again is not a change: no notification.
	if err := st.SetStatus(ctx, d1.ID, store.StatusDeploying); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c1:
		t.Fatal("woken by an UPDATE that did not change the status")
	case <-time.After(300 * time.Millisecond):
	}

	// Kill the listen connection; after pq reconnects, all subscribers are
	// woken so they re-read whatever they may have missed.
	raw, err := sql.Open("postgres", os.Getenv("PAAS_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var killed int
	if err := raw.QueryRowContext(ctx, `
		SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND query LIKE 'LISTEN%'`).Scan(&killed); err != nil {
		t.Fatal(err)
	}
	if killed == 0 {
		t.Fatal("listen connection not found")
	}
	waitWake(t, c2, "reconnect")

	// And it listens again.
	drain(c1)
	if err := st.AppendLog(ctx, d1.ID, "after reconnect"); err != nil {
		t.Fatal(err)
	}
	waitWake(t, c1, "AppendLog after reconnect")
}

func TestLatestDeployments(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	a, _ := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	b, _ := st.CreateApp(ctx, "shop", "nisagwn/shop", "main")
	st.EnqueueDeployment(ctx, a.ID, sha(1), "main", "")
	last, _, _ := st.EnqueueDeployment(ctx, a.ID, sha(2), "main", "")

	got, err := st.LatestDeployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got[a.ID].ID != last.ID {
		t.Errorf("latest of blog = %d, want %d", got[a.ID].ID, last.ID)
	}
	if _, ok := got[b.ID]; ok {
		t.Error("app without deployments has a latest deployment")
	}
}
