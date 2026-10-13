package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

// Faz 28: a deletion hides the app, stops its deployments, removes every
// row that references it in one step, keeps the audit log and notification
// history by name, and frees the name once the cluster step finished.
func TestAppDeletion(t *testing.T) {
	st := testdb.Open(t)
	db := rawDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	alice, _ := st.UpsertUser(ctx, 1, "alice", "", "")
	team, err := st.CreateTeam(ctx, "web", "Web", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateAppInTeam(ctx, team.ID, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := st.CreateAppInTeam(ctx, team.ID, "shop", "nisagwn/shop", "main")

	// Something in every table that references the app.
	prod := []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}}
	ready, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "one")
	if err := st.MarkReady(ctx, ready, prod); err != nil {
		t.Fatal(err)
	}
	st.AppendLog(ctx, ready.ID, "built")
	running, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(2), "feature", "two")
	if _, err := st.ClaimNext(ctx); err != nil { // claims "running" (the oldest queued)
		t.Fatal(err)
	}
	queued, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(3), "feature", "three")
	v := "x"
	if err := st.ApplyEnvChanges(ctx, app.ID, []store.EnvChange{{Key: "KEY", Value: &v}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDomain(ctx, app.ID, "www.example.com", "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployHook(ctx, app.ID, "cms", "main", sha(9)[:40]+sha(9)[:24], "paas_hook_x", alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetProtection(ctx, app.ID, "team", "previews", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateShareLink(ctx, app.ID, "s", sha(8)[:40]+sha(8)[:24], "p", alice.ID, nil); err != nil {
		t.Fatal(err)
	}
	addon := mustAddon(t, st, app.ID, "postgres", "db", "copy")
	exec(`INSERT INTO addon_backups (addon_id, job, trigger) VALUES ($1, 'b-1', 'manual')`, addon.ID)
	exec(`INSERT INTO app_build_settings (app_id, framework) VALUES ($1, 'vite')`, app.ID)
	exec(`INSERT INTO app_process_scale (app_id, process, replicas) VALUES ($1, 'worker', 2)`, app.ID)
	exec(`INSERT INTO rollout_settings (app_id, mode) VALUES ($1, 'guarded')`, app.ID)
	exec(`INSERT INTO request_metrics (deployment_id, app_id, minute, requests) VALUES ($1, $2, now(), 5)`, ready.ID, app.ID)
	if _, err := st.CreateThread(ctx, ready, store.NewComment{AuthorID: &alice.ID, AuthorLogin: "alice", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartAudit(ctx, store.AuditEntry{Action: store.AuditTerminal, AppID: app.ID, DeploymentID: &ready.ID,
		UserID: &alice.ID, Actor: "alice", Process: "web", Pod: "p"}); err != nil {
		t.Fatal(err)
	}
	var onlyBlog, both int64
	db.QueryRowContext(ctx, `INSERT INTO notification_channels (team_id, name, kind) VALUES ($1, 'only-blog', 'slack') RETURNING id`, team.ID).Scan(&onlyBlog)
	db.QueryRowContext(ctx, `INSERT INTO notification_channels (team_id, name, kind) VALUES ($1, 'both', 'slack') RETURNING id`, team.ID).Scan(&both)
	exec(`INSERT INTO notification_channel_apps (channel_id, app_id) VALUES ($1, $3), ($2, $3), ($2, $4)`, onlyBlog, both, app.ID, other.ID)
	if _, _, err := st.EnqueueNotification(ctx, store.NotificationEventInput{TeamID: team.ID, AppID: app.ID,
		Type: "deployment.failed", Key: "k", Payload: []byte(`{"app":"blog"}`)}); err != nil {
		t.Fatal(err)
	}

	// Step 1.
	by := store.DeletionActor{Login: "alice", UserID: &alice.ID}
	del, err := st.BeginAppDeletion(ctx, app.ID, by)
	if err != nil {
		t.Fatal(err)
	}
	if del.AppName != "blog" || del.Repo != "nisagwn/blog" || del.RequestedBy != "alice" || del.FinishedAt != nil {
		t.Fatalf("deletion %+v", del)
	}
	if again, err := st.BeginAppDeletion(ctx, app.ID, by); err != nil || again.ID != del.ID {
		t.Fatalf("begin twice: %+v %v", again, err)
	}
	if _, err := st.GetAppByName(ctx, "blog"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAppByName while deleting: %v", err)
	}
	if _, err := st.GetAppByRepo(ctx, "nisagwn/blog"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAppByRepo while deleting: %v", err)
	}
	if apps, _ := st.AppsForUser(ctx, alice.ID); len(apps) != 1 || apps[0].Name != "shop" {
		t.Fatalf("AppsForUser: %+v", apps)
	}
	if apps, _ := st.ListApps(ctx); len(apps) != 1 {
		t.Fatalf("ListApps: %+v", apps)
	}
	if u, err := st.TeamUsage(ctx, team.ID, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil || u.Projects != 1 {
		t.Fatalf("usage while deleting: %+v %v", u, err)
	}
	if _, _, err := st.EnqueueDeployment(ctx, app.ID, sha(4), "main", "late push"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("enqueue while deleting: %v", err)
	}
	if _, err := st.TriggerDeployHook(ctx, sha(9)[:40]+sha(9)[:24]); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hook while deleting: %v", err)
	}
	if d, _ := st.GetDeployment(ctx, queued.ID); d.Status != store.StatusCanceled || d.Error != store.DeletedReason {
		t.Fatalf("queued: %+v", d)
	}
	if d, _ := st.GetDeployment(ctx, running.ID); d.Status != store.StatusBuilding || d.CancelRequestedAt == nil {
		t.Fatalf("running: %+v", d)
	}
	if _, err := st.CreateAppInTeam(ctx, team.ID, "blog", "nisagwn/blog2", "main"); !errors.Is(err, store.ErrAppDeleting) || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("create while deleting: %v", err)
	}

	// Step 2 waits for the running deployment.
	if err := st.DeleteAppRows(ctx, del); !errors.Is(err, store.ErrDeploymentsRunning) {
		t.Fatalf("rows with a running deployment: %v", err)
	}
	if err := st.MarkFailed(ctx, running.ID, "context canceled"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAppRows(ctx, del); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAppRows(ctx, del); err != nil { // repeated after a crash
		t.Fatalf("repeat: %v", err)
	}
	// No table keeps a row of the app (audit_log and notification_events
	// keep theirs with app_id NULL; app_deletions is the record itself).
	rows, err := db.QueryContext(ctx, `SELECT table_name FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name = 'app_id' AND table_name <> 'app_deletions'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) < 15 {
		t.Fatalf("tables with app_id: %v", tables)
	}
	for _, tbl := range tables {
		if n := count(`SELECT count(*) FROM `+tbl+` WHERE app_id = $1`, app.ID); n != 0 {
			t.Errorf("%s still has %d row(s) of the deleted app", tbl, n)
		}
	}
	for _, tbl := range []string{"deployment_logs", "deployment_processes"} {
		if n := count(`SELECT count(*) FROM `+tbl+` WHERE deployment_id = ANY($1)`, pqArray(ready.ID, running.ID, queued.ID)); n != 0 {
			t.Errorf("%s still has %d row(s)", tbl, n)
		}
	}
	if n := count(`SELECT count(*) FROM addon_backups WHERE addon_id = $1`, addon.ID); n != 0 {
		t.Errorf("addon backups: %d", n)
	}
	if n := count(`SELECT count(*) FROM audit_log WHERE app_id IS NULL AND app_name = 'blog'`); n != 2 {
		t.Errorf("audit entries kept by name: %d, want terminal + app_delete", n)
	}
	if n := count(`SELECT count(*) FROM audit_log WHERE action = $1 AND actor = 'alice' AND user_id = $2`, store.AuditAppDelete, alice.ID); n != 1 {
		t.Errorf("app_delete audit entry: %d", n)
	}
	if n := count(`SELECT count(*) FROM notification_events WHERE team_id = $1 AND app_id IS NULL`, team.ID); n != 1 {
		t.Errorf("notification history: %d", n)
	}
	var enabledOnly, enabledBoth bool
	db.QueryRowContext(ctx, `SELECT enabled FROM notification_channels WHERE id = $1`, onlyBlog).Scan(&enabledOnly)
	db.QueryRowContext(ctx, `SELECT enabled FROM notification_channels WHERE id = $1`, both).Scan(&enabledBoth)
	if enabledOnly || !enabledBoth {
		t.Errorf("channels: only-blog enabled %v (want false), both enabled %v (want true)", enabledOnly, enabledBoth)
	}
	if _, err := st.GetAppByID(ctx, app.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("app row: %v", err)
	}

	// The name stays taken until the cluster step finished.
	if _, err := st.CreateAppInTeam(ctx, team.ID, "blog", "nisagwn/blog", "main"); !errors.Is(err, store.ErrAppDeleting) {
		t.Fatalf("create before finish: %v", err)
	}
	if pending, _ := st.PendingAppDeletions(ctx); len(pending) != 1 || pending[0].RowsDeletedAt == nil {
		t.Fatalf("pending: %+v", pending)
	}
	if ok, err := st.ClaimAppDeletion(ctx, del.ID, time.Minute); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, _ := st.ClaimAppDeletion(ctx, del.ID, time.Minute); ok {
		t.Fatal("second claim succeeded")
	}
	if err := st.FinishAppDeletion(ctx, del.ID); err != nil {
		t.Fatal(err)
	}
	if pending, _ := st.PendingAppDeletions(ctx); len(pending) != 0 {
		t.Fatalf("pending after finish: %+v", pending)
	}
	// Name and repo are free again: a reimport of the same repo works.
	again, err := st.CreateAppInTeam(ctx, team.ID, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatalf("reuse name: %v", err)
	}
	if again.ID == app.ID {
		t.Fatal("same id")
	}
	if d, created, err := st.EnqueueDeployment(ctx, again.ID, sha(1), "main", "one"); err != nil || !created {
		t.Fatalf("deploy the reimported app: %+v %v %v", d, created, err)
	}
	if u, _ := st.TeamUsage(ctx, team.ID, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); u.Projects != 2 {
		t.Fatalf("usage after reimport: %+v", u)
	}
	// The finished record is history.
	if got, err := st.GetAppDeletion(ctx, del.ID); err != nil || got.FinishedAt == nil || got.AppID != app.ID {
		t.Fatalf("record: %+v %v", got, err)
	}
	if _, err := st.BeginAppDeletion(ctx, app.ID, by); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("begin on a deleted app: %v", err)
	}
}

func pqArray(ids ...int64) any {
	out := "{"
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += itoa(id)
	}
	return out + "}"
}
