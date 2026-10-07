package store_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func mustAddon(t *testing.T, st *store.Store, appID int64, kind, name, mode string) store.Addon {
	t.Helper()
	a, err := st.CreateAddon(context.Background(), appID, store.NewAddon{Kind: kind, Name: name, Plan: "hobby", PreviewMode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func ready(t *testing.T, st *store.Store, a store.Addon) store.Addon {
	t.Helper()
	ctx := context.Background()
	if err := st.SetAddonState(ctx, a.ID, store.AddonReady, ""); err != nil {
		t.Fatal(err)
	}
	a, err := st.GetAddonByID(ctx, a.ID)
	if err != nil || a.ReadyAt == nil {
		t.Fatalf("ready: %+v %v", a, err)
	}
	return a
}

func TestAddonsCreateAndSecrets(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	st.SetEnvKeyring(keyring(t, genKey(t)))
	app := mustApp(t, st, "blog")

	pg := mustAddon(t, st, app.ID, store.AddonPostgres, "db", store.PreviewCopy)
	if pg.Status != store.AddonProvisioning || pg.AppName != "blog" || pg.Host() != "addon-db.app-blog.svc" ||
		pg.Port() != 5432 || pg.EnvPrefix() != "" || len(pg.Anonymize) != 0 || pg.BackupKeep != store.DefaultBackupKeep {
		t.Fatalf("created: %+v", pg)
	}
	if _, err := st.CreateAddon(ctx, app.ID, store.NewAddon{Kind: "postgres", Name: "db", Plan: "hobby", PreviewMode: "copy"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := st.CreateAddon(ctx, app.ID, store.NewAddon{Kind: "redis", Name: "c2", Plan: "hobby", PreviewMode: "copy"}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("redis with copy mode: %v", err)
	}
	for i := 0; i < store.MaxAddonsPerApp-1; i++ {
		mustAddon(t, st, app.ID, store.AddonRedis, "cache"+string(rune('a'+i)), store.PreviewShared)
	}
	if _, err := st.CreateAddon(ctx, app.ID, store.NewAddon{Kind: "redis", Name: "extra", Plan: "hobby", PreviewMode: "shared"}); !errors.Is(err, store.ErrAddonLimit) {
		t.Fatalf("limit: %v", err)
	}

	// Credentials are sealed: no plaintext in the row, readable through the store.
	cur, next, err := st.AddonSecretsOf(ctx, pg)
	if err != nil || len(cur.Password) != 40 || len(cur.Admin) != 40 || cur.Admin == cur.Password || next != nil {
		t.Fatalf("secrets: %+v %v %v", cur, next, err)
	}
	var stored, keyID string
	if err := rawDB(t).QueryRow(`SELECT secrets, key_id FROM addons WHERE id = $1`, pg.ID).Scan(&stored, &keyID); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, cur.Password) || !strings.HasPrefix(stored, "v1:") || keyID == "" {
		t.Fatalf("stored secrets are not sealed: %q", stored)
	}
	// The ciphertext is bound to the app and add-on name.
	if err := st.Exec(ctx, `UPDATE addons SET secrets = $1 WHERE app_id = $2 AND name = 'cachea'`, stored, app.ID); err != nil {
		t.Fatal(err)
	}
	other, _ := st.GetAddon(ctx, app.ID, "cachea")
	if _, _, err := st.AddonSecretsOf(ctx, other); err == nil {
		t.Fatal("a ciphertext moved to another add-on decrypted")
	}

	conn, err := st.ProductionConnection(ctx, pg)
	if err != nil || conn.URL() != "postgres://app:"+cur.Password+"@addon-db.app-blog.svc:5432/app?sslmode=disable" {
		t.Fatalf("connection: %+v %v", conn, err)
	}

	// Settings change; a deleting add-on cannot be changed.
	rules := []string{"users.email: email"}
	plan, keep, sql := "standard", 3, "UPDATE t SET a = 1;"
	pg, err = st.UpdateAddon(ctx, pg, store.AddonChange{Plan: &plan, Anonymize: &rules, AnonymizeSQL: &sql, BackupKeep: &keep})
	if err != nil || pg.Plan != "standard" || pg.Anonymize[0] != "users.email: email" || pg.AnonymizeSQL != sql || pg.BackupKeep != 3 {
		t.Fatalf("update: %+v %v", pg, err)
	}
	if pg, err = st.DeleteAddon(ctx, pg); err != nil || pg.Status != store.AddonDeleting {
		t.Fatalf("delete: %+v %v", pg, err)
	}
	if err := st.SetAddonState(ctx, pg.ID, store.AddonReady, ""); err != nil {
		t.Fatal(err)
	}
	if pg, _ = st.GetAddonByID(ctx, pg.ID); pg.Status != store.AddonDeleting {
		t.Fatal("a deleting add-on came back")
	}
	if _, err := st.UpdateAddon(ctx, pg, store.AddonChange{Plan: &plan}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update while deleting: %v", err)
	}
	if err := st.RemoveAddon(ctx, pg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAddonByID(ctx, pg.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("row not removed")
	}

	// Key rotation re-seals add-on credentials too.
	stats, _ := st.EnvKeyStats(ctx)
	if stats[keyID] != store.MaxAddonsPerApp-1 {
		t.Fatalf("key stats: %v", stats)
	}
}

func TestAddonEnv(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	db := ready(t, st, mustAddon(t, st, app.ID, store.AddonPostgres, "db", store.PreviewCopy))
	analytics := ready(t, st, mustAddon(t, st, app.ID, store.AddonPostgres, "analytics", store.PreviewShared))
	cache := ready(t, st, mustAddon(t, st, app.ID, store.AddonRedis, "cache", store.PreviewShared))
	mustAddon(t, st, app.ID, store.AddonRedis, "queue", store.PreviewShared) // provisioning: no variables yet

	if err := st.UpdateAppEnv(ctx, app.ID, map[string]*string{"PGPORT": strp("6543")}); err != nil {
		t.Fatal(err)
	}
	prod, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "")
	env, err := st.DeploymentEnv(ctx, prod)
	if err != nil {
		t.Fatal(err)
	}
	dbSec, _, _ := st.AddonSecretsOf(ctx, db)
	anSec, _, _ := st.AddonSecretsOf(ctx, analytics)
	cacheSec, _, _ := st.AddonSecretsOf(ctx, cache)
	want := map[string]string{
		"DATABASE_URL":           "postgres://app:" + dbSec.Password + "@addon-db.app-blog.svc:5432/app?sslmode=disable",
		"PGHOST":                 "addon-db.app-blog.svc",
		"PGPORT":                 "6543", // the user's variable wins
		"PGUSER":                 "app",
		"PGPASSWORD":             dbSec.Password,
		"PGDATABASE":             "app",
		"ANALYTICS_DATABASE_URL": "postgres://app:" + anSec.Password + "@addon-analytics.app-blog.svc:5432/app?sslmode=disable",
		"ANALYTICS_PGHOST":       "addon-analytics.app-blog.svc",
		"ANALYTICS_PGPASSWORD":   anSec.Password,
		"REDIS_URL":              "redis://default:" + cacheSec.Password + "@addon-cache.app-blog.svc:6379",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if _, ok := env["QUEUE_REDIS_URL"]; ok {
		t.Error("a provisioning add-on gave variables")
	}

	// A preview gets its branch database once its copy is ready; analytics
	// (shared) stays on production.
	preview, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(2), "feature/x", "")
	env, _ = st.DeploymentEnv(ctx, preview)
	if _, ok := env["DATABASE_URL"]; ok || env["ANALYTICS_PGDATABASE"] != "app" {
		t.Fatalf("preview without a ready copy: %v", env)
	}
	b, err := st.EnsureAddonBranch(ctx, db, "feature/x")
	if err != nil || b.Status != store.BranchPending || b.Database != "preview_feature_x_"+b.Database[len(b.Database)-8:] || b.Mode != store.PreviewCopy {
		t.Fatalf("branch: %+v %v", b, err)
	}
	if ok, _ := st.StartAddonBranch(ctx, b.ID, b.Generation); !ok {
		t.Fatal("start")
	}
	if ok, _ := st.FinishAddonBranch(ctx, b.ID, b.Generation, store.BranchResult{Mode: store.PreviewCopy}); !ok {
		t.Fatal("finish")
	}
	env, _ = st.DeploymentEnv(ctx, preview)
	pws, _ := st.AddonBranchPasswords(ctx, db)
	if env["PGDATABASE"] != b.Database || env["PGUSER"] != b.Database || env["PGPASSWORD"] != pws[b.Database] ||
		pws[b.Database] == dbSec.Password || !strings.Contains(env["DATABASE_URL"], "/"+b.Database+"?") {
		t.Fatalf("preview env: %v", env)
	}
	// A promoted copy of the preview runs with production's database.
	promoted := preview
	promoted.Target = store.EnvProduction
	if env, _ = st.DeploymentEnv(ctx, promoted); env["PGDATABASE"] != "app" {
		t.Fatalf("promoted: %v", env)
	}
}

func TestAddonBranchLifecycle(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	db := ready(t, st, mustAddon(t, st, app.ID, store.AddonPostgres, "db", store.PreviewCopy))

	b, _ := st.EnsureAddonBranch(ctx, db, "dev")
	if again, err := st.EnsureAddonBranch(ctx, db, "dev"); err != nil || again.ID != b.ID || again.Generation != 0 {
		t.Fatalf("ensure is idempotent: %+v %v", again, err)
	}
	if _, err := st.ResetAddonBranch(ctx, db, "dev"); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("reset while pending: %v", err)
	}
	st.StartAddonBranch(ctx, b.ID, 0)
	if ok, _ := st.FailAddonBranch(ctx, b.ID, 0, "relation does not exist"); !ok {
		t.Fatal("fail")
	}
	// A new push queues a failed copy again, with the next generation.
	b, _ = st.EnsureAddonBranch(ctx, db, "dev")
	if b.Status != store.BranchPending || b.Generation != 1 || b.Error != "" {
		t.Fatalf("retry: %+v", b)
	}
	st.StartAddonBranch(ctx, b.ID, 1)
	// A result of an older generation is dropped.
	if ok, _ := st.FinishAddonBranch(ctx, b.ID, 0, store.BranchResult{}); ok {
		t.Fatal("stale result applied")
	}
	snap := time.Now().UTC().Truncate(time.Second)
	size := int64(4096)
	if ok, _ := st.FinishAddonBranch(ctx, b.ID, 1, store.BranchResult{Mode: store.PreviewEmpty, Warning: "too big", SnapshotAt: &snap, SizeBytes: &size}); !ok {
		t.Fatal("finish")
	}
	b, _ = st.GetAddonBranch(ctx, db.ID, "dev")
	if b.Status != store.BranchReady || b.Mode != store.PreviewEmpty || b.Warning != "too big" || *b.SizeBytes != 4096 || !b.SnapshotAt.Equal(snap) {
		t.Fatalf("ready: %+v", b)
	}
	if b, err := st.ResetAddonBranch(ctx, db, "dev"); err != nil || b.Status != store.BranchPending || b.Generation != 2 || b.Mode != store.PreviewCopy {
		t.Fatalf("reset: %+v %v", b, err)
	}
	if _, err := st.ResetAddonBranch(ctx, db, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reset unknown: %v", err)
	}

	// Garbage collection: no live preview deployment → deleting (after the grace).
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(3), "dev", "")
	st.StartAddonBranch(ctx, b.ID, 2)
	st.FinishAddonBranch(ctx, b.ID, 2, store.BranchResult{Mode: store.PreviewCopy})
	if got, _ := st.ExpireAddonBranches(ctx, db, 0); len(got) != 0 {
		t.Fatalf("expired with a queued deployment: %v", got)
	}
	st.Exec(ctx, `UPDATE deployments SET status = 'failed' WHERE id = $1`, d.ID)
	if got, _ := st.ExpireAddonBranches(ctx, db, time.Hour); len(got) != 0 {
		t.Fatal("expired within the grace")
	}
	if got, _ := st.ExpireAddonBranches(ctx, db, 0); len(got) != 1 || got[0] != "dev" {
		t.Fatalf("expire: %v", got)
	}
	if b, _ = st.GetAddonBranch(ctx, db.ID, "dev"); b.Status != store.BranchDeleting {
		t.Fatalf("status %s", b.Status)
	}
	// Ensure leaves a row being dropped alone; the waiting deployment asks again later.
	if again, _ := st.EnsureAddonBranch(ctx, db, "dev"); again.Status != store.BranchDeleting {
		t.Fatal("ensure revived a deleting row")
	}
	if err := st.RemoveAddonBranch(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.EnsureAddonBranch(ctx, db, "dev"); again.ID == b.ID || again.Status != store.BranchPending {
		t.Fatalf("recreated: %+v", again)
	}

	// Branch deletion marks the branch's databases in the same transaction.
	st.EnsureAddonBranch(ctx, db, "feature/y")
	bc, err := st.DeleteBranch(ctx, app.ID, "feature/y", "branch deleted")
	if err != nil || bc.Databases != 1 {
		t.Fatalf("delete branch: %+v %v", bc, err)
	}
	if y, _ := st.GetAddonBranch(ctx, db.ID, "feature/y"); y.Status != store.BranchDeleting {
		t.Fatalf("feature/y: %s", y.Status)
	}

	// Switching to shared drops every copy.
	if err := st.DeleteAddonBranches(ctx, db); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.ListAddonBranches(ctx, db.ID)
	for _, r := range rows {
		if r.Status != store.BranchDeleting {
			t.Errorf("%s: %s", r.Branch, r.Status)
		}
	}
}

func TestAddonRotation(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	st.SetEnvKeyring(keyring(t, genKey(t)))
	app := mustApp(t, st, "blog")
	db := ready(t, st, mustAddon(t, st, app.ID, store.AddonPostgres, "db", store.PreviewCopy))
	old, _, _ := st.AddonSecretsOf(ctx, db)

	db, err := st.RequestAddonRotation(ctx, db, true)
	if err != nil || !db.Rotating {
		t.Fatalf("request: %+v %v", db, err)
	}
	if _, err := st.RequestAddonRotation(ctx, db, true); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("second request: %v", err)
	}
	cur, next, _ := st.AddonSecretsOf(ctx, db)
	if cur != old || next == nil || next.Password == old.Password || next.Admin != old.Admin {
		t.Fatalf("pending: %+v %+v", cur, next)
	}
	redeploy, err := st.CompleteAddonRotation(ctx, db)
	if err != nil || !redeploy {
		t.Fatalf("complete: %v %v", redeploy, err)
	}
	db, _ = st.GetAddonByID(ctx, db.ID)
	cur2, next2, _ := st.AddonSecretsOf(ctx, db)
	if cur2.Password != next.Password || next2 != nil || db.Rotating || db.SecretsVersion != 2 {
		t.Fatalf("after: %+v %+v %+v", cur2, next2, db)
	}

	// A failed rotation keeps the current password.
	db, _ = st.RequestAddonRotation(ctx, db, false)
	if err := st.CancelAddonRotation(ctx, db, "rotation failed"); err != nil {
		t.Fatal(err)
	}
	db, _ = st.GetAddonByID(ctx, db.ID)
	if cur3, n, _ := st.AddonSecretsOf(ctx, db); cur3 != cur2 || n != nil || db.Message != "rotation failed" {
		t.Fatalf("canceled: %+v %v", db, n)
	}

	// Alias targets: what a rotation redeploys.
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "")
	st.MarkReady(ctx, d, []store.AliasSpec{{Hostname: "blog.x", Kind: store.AliasProduction, Branch: "main"},
		{Hostname: "main-blog.x", Kind: store.AliasPreview, Branch: "main"}})
	if ts, err := st.AliasTargets(ctx, app.ID); err != nil || len(ts) != 1 || ts[0].ID != d.ID {
		t.Fatalf("alias targets: %+v %v", ts, err)
	}

	// Re-encryption under a new key keeps every credential readable.
	k2 := genKey(t)
	st.SetEnvKeyring(keyring(t, k2, genKey(t)))
	if _, _, err := st.AddonSecretsOf(ctx, db); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}

func TestAddonReencrypt(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	// Written without a keyring: plaintext.
	db := ready(t, st, mustAddon(t, st, app.ID, store.AddonPostgres, "db", store.PreviewCopy))
	st.EnsureAddonBranch(ctx, db, "dev")
	plain, _, _ := st.AddonSecretsOf(ctx, db)
	pws, _ := st.AddonBranchPasswords(ctx, db)

	k1 := genKey(t)
	st.SetEnvKeyring(keyring(t, k1))
	n, err := st.ReencryptEnv(ctx)
	if err != nil || n != 2 {
		t.Fatalf("reencrypt: %d %v", n, err)
	}
	if got, _, err := st.AddonSecretsOf(ctx, db); err != nil || got != plain {
		t.Fatalf("after reencrypt: %+v %v", got, err)
	}
	if got, err := st.AddonBranchPasswords(ctx, db); err != nil || got["preview_dev"] != pws["preview_dev"] {
		t.Fatalf("branch passwords: %v %v", got, err)
	}
	if err := st.CheckEnvKeys(ctx); err != nil {
		t.Fatal(err)
	}
	st.SetEnvKeyring(nil)
	if err := st.CheckEnvKeys(ctx); err == nil {
		t.Fatal("sealed add-on rows without a key passed the check")
	}
}

func TestAddonBackups(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	db := ready(t, st, mustAddon(t, st, app.ID, store.AddonPostgres, "db", store.PreviewCopy))
	cache := ready(t, st, mustAddon(t, st, app.ID, store.AddonRedis, "cache", store.PreviewShared))
	if _, err := st.CreateManualBackup(ctx, cache); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("redis backup: %v", err)
	}
	m, err := st.CreateManualBackup(ctx, db)
	if err != nil || m.Job != "addon-db-backup-m"+itoa(m.ID) || m.Status != store.BackupPending || m.Trigger != store.BackupManual {
		t.Fatalf("manual: %+v %v", m, err)
	}
	if _, err := st.CreateManualBackup(ctx, db); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("second manual: %v", err)
	}
	if err := st.SetBackupStatus(ctx, m.ID, store.BackupRunning, ""); err != nil {
		t.Fatal(err)
	}

	// Runs of the CronJob are imported by job name; a finished row never moves back.
	t1 := time.Now().Add(-48 * time.Hour).UTC()
	t2 := time.Now().Add(-24 * time.Hour).UTC()
	size := int64(100)
	for _, r := range []store.BackupRun{
		{Job: "addon-db-backup-1", Trigger: store.BackupScheduled, Status: store.BackupRunning, StartedAt: &t1},
		{Job: "addon-db-backup-1", Trigger: store.BackupScheduled, Status: store.BackupSucceeded, SizeBytes: &size, StartedAt: &t1, FinishedAt: &t1},
		{Job: "addon-db-backup-1", Trigger: store.BackupScheduled, Status: store.BackupRunning},
		{Job: "addon-db-backup-2", Trigger: store.BackupScheduled, Status: store.BackupSucceeded, SizeBytes: &size, StartedAt: &t2, FinishedAt: &t2},
		{Job: m.Job, Trigger: store.BackupManual, Status: store.BackupSucceeded, SizeBytes: &size, FinishedAt: &t2},
	} {
		if err := st.RecordBackupRun(ctx, db.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := st.ListAddonBackups(ctx, db.ID, 10)
	if len(list) != 3 || list[len(list)-1].Job != "addon-db-backup-1" || list[len(list)-1].Status != store.BackupSucceeded {
		t.Fatalf("list: %+v", list)
	}
	// The latest backup kept two files: backup-1 was pruned.
	if err := st.ExpireAddonBackups(ctx, db.ID, []string{"addon-db-backup-2.dump", m.Job + ".dump"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	b1 := list[len(list)-1]
	if b1, _ = st.GetAddonBackup(ctx, db.ID, b1.ID); b1.Status != store.BackupExpired {
		t.Fatalf("pruned backup: %s", b1.Status)
	}

	// Restore: only a succeeded backup, one at a time.
	if _, err := st.RequestRestore(ctx, db, b1.ID, "bob"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("restore of a pruned backup: %v", err)
	}
	r, err := st.RequestRestore(ctx, db, m.ID, "bob")
	if err != nil || r.RestoreStatus != store.BackupPending || r.RestoreBy != "bob" {
		t.Fatalf("restore: %+v %v", r, err)
	}
	b2 := list[1]
	if _, err := st.RequestRestore(ctx, db, b2.ID, "bob"); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("second restore: %v", err)
	}
	st.SetRestoreStatus(ctx, m.ID, store.BackupRunning, "")
	st.SetRestoreStatus(ctx, m.ID, store.BackupSucceeded, "")
	if r, _ = st.GetAddonBackup(ctx, db.ID, m.ID); r.RestoreStatus != store.BackupSucceeded || r.RestoreFinishedAt == nil {
		t.Fatalf("restored: %+v", r)
	}
	if _, err := st.RequestRestore(ctx, db, b2.ID, "bob"); err != nil {
		t.Fatalf("restore after the first finished: %v", err)
	}
}

func TestClaimAddons(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	a := mustAddon(t, st, app.ID, store.AddonPostgres, "db", store.PreviewCopy)
	got, err := st.ClaimDueAddons(ctx, time.Minute)
	if err != nil || len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("claim: %+v %v", got, err)
	}
	if got, _ := st.ClaimDueAddons(ctx, time.Minute); len(got) != 0 {
		t.Fatalf("claimed twice: %+v", got)
	}
	if got, _ := st.ClaimAppAddons(ctx, app.ID, time.Minute); len(got) != 0 {
		t.Fatal("kick within the gap")
	}
	// A change makes it due at once.
	keep := 2
	st.UpdateAddon(ctx, a, store.AddonChange{BackupKeep: &keep})
	if got, _ := st.ClaimAppAddons(ctx, app.ID, time.Minute); len(got) != 1 {
		t.Fatal("changed add-on not due")
	}
}

func strp(s string) *string { return &s }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
