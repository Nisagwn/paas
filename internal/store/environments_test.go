package store_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/secret"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func str(s string) *string { return &s }

func applyEnv(t *testing.T, st *store.Store, appID int64, changes ...store.EnvChange) {
	t.Helper()
	if err := st.ApplyEnvChanges(context.Background(), appID, changes); err != nil {
		t.Fatal(err)
	}
}

func resolve(t *testing.T, st *store.Store, appID int64, target, branch string) map[string]string {
	t.Helper()
	env, err := st.ResolveEnv(context.Background(), appID, target, branch)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func sameEnv(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: env = %v, want %v", what, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %s = %q, want %q (env %v)", what, k, got[k], v, got)
		}
	}
}

// Production gets all < production; preview gets all < preview < preview
// of the branch. Values are encrypted with the target in the AAD.
func TestEnvTargets(t *testing.T) {
	st := testdb.Open(t)
	db := rawDB(t)
	ctx := context.Background()
	k1 := genKey(t)
	keys := keyring(t, k1)
	st.SetEnvKeyring(keys)
	app := mustApp(t, st, "blog")

	applyEnv(t, st, app.ID,
		store.EnvChange{Key: "SHARED", Value: str("both")},
		store.EnvChange{Key: "DB_URL", Value: str("postgres://prod"), Target: store.EnvProduction},
		store.EnvChange{Key: "DB_URL", Value: str("postgres://preview"), Target: store.EnvPreview},
		store.EnvChange{Key: "DB_URL", Value: str("postgres://feature"), Target: store.EnvPreview, GitBranch: "feature/x"},
		store.EnvChange{Key: "ONLY_PREVIEW", Value: str("1"), Target: store.EnvPreview},
	)
	sameEnv(t, "production", resolve(t, st, app.ID, store.EnvProduction, "main"),
		map[string]string{"SHARED": "both", "DB_URL": "postgres://prod"})
	sameEnv(t, "preview", resolve(t, st, app.ID, store.EnvPreview, "other"),
		map[string]string{"SHARED": "both", "DB_URL": "postgres://preview", "ONLY_PREVIEW": "1"})
	sameEnv(t, "preview of feature/x", resolve(t, st, app.ID, store.EnvPreview, "feature/x"),
		map[string]string{"SHARED": "both", "DB_URL": "postgres://feature", "ONLY_PREVIEW": "1"})
	// A deployment resolves through its own target and branch.
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("a", 40), "feature/x", "")
	if d.Target != store.EnvPreview || d.Origin != store.OriginGit || d.Generation != 0 {
		t.Fatalf("deployment of a feature branch: %+v", d)
	}
	env, err := st.DeploymentEnv(ctx, d)
	if err != nil || env["DB_URL"] != "postgres://feature" {
		t.Fatalf("DeploymentEnv: %v %v", env, err)
	}
	if p, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("b", 40), "main", ""); p.Target != store.EnvProduction {
		t.Fatalf("main deployment target = %q", p.Target)
	}

	// Every row is ciphertext under the current key.
	rows, err := db.Query(`SELECT value, key_id FROM app_env WHERE app_id = $1`, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var v, id string
		rows.Scan(&v, &id)
		if id != keys.CurrentID() || strings.Contains(v, "postgres://") {
			t.Fatalf("row not encrypted: %q %q", v, id)
		}
		n++
	}
	rows.Close()
	if n != 5 {
		t.Fatalf("%d rows, want 5", n)
	}

	// A preview ciphertext moved into the production row does not decrypt.
	if _, err := db.Exec(`UPDATE app_env SET value = (SELECT value FROM app_env
		WHERE app_id = $1 AND key = 'DB_URL' AND target = 'preview' AND git_branch = '')
		WHERE app_id = $1 AND key = 'DB_URL' AND target = 'production'`, app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResolveEnv(ctx, app.ID, store.EnvProduction, "main"); !errors.Is(err, secret.ErrDecrypt) {
		t.Fatalf("swapped target: err = %v, want ErrDecrypt", err)
	}
	applyEnv(t, st, app.ID, store.EnvChange{Key: "DB_URL", Value: str("postgres://prod"), Target: store.EnvProduction})

	// Rotation re-encrypts scoped rows with their own AAD.
	k2 := genKey(t)
	st.SetEnvKeyring(keyring(t, k2, k1))
	if n, err := st.ReencryptEnv(ctx); err != nil || n != 5 {
		t.Fatalf("rotate: n=%d err=%v", n, err)
	}
	sameEnv(t, "production after rotation", resolve(t, st, app.ID, store.EnvProduction, "main"),
		map[string]string{"SHARED": "both", "DB_URL": "postgres://prod"})
	sameEnv(t, "feature after rotation", resolve(t, st, app.ID, store.EnvPreview, "feature/x"),
		map[string]string{"SHARED": "both", "DB_URL": "postgres://feature", "ONLY_PREVIEW": "1"})

	// No target: both environments. Setting replaces the production and
	// preview rows (branch overrides stay); deleting removes every row.
	applyEnv(t, st, app.ID, store.EnvChange{Key: "DB_URL", Value: str("postgres://one")})
	sameEnv(t, "production after a both-environments set", resolve(t, st, app.ID, store.EnvProduction, "main"),
		map[string]string{"SHARED": "both", "DB_URL": "postgres://one"})
	if got := resolve(t, st, app.ID, store.EnvPreview, "feature/x")["DB_URL"]; got != "postgres://feature" {
		t.Fatalf("branch override after a both-environments set: %q", got)
	}
	if err := st.UpdateAppEnv(ctx, app.ID, map[string]*string{"DB_URL": nil}); err != nil {
		t.Fatal(err)
	}
	if _, ok := resolve(t, st, app.ID, store.EnvPreview, "feature/x")["DB_URL"]; ok {
		t.Fatal("DB_URL survived a both-environments delete")
	}
	vars, _ := st.ListEnv(ctx, app.ID)
	if len(vars) != 2 {
		t.Fatalf("ListEnv = %+v", vars)
	}

	// A branch needs the preview target.
	if err := st.ApplyEnvChanges(ctx, app.ID, []store.EnvChange{{Key: "X", Value: str("1"),
		Target: store.EnvProduction, GitBranch: "main"}}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("production with a branch: %v", err)
	}
}

// Rows written before migration 011 (encrypted with the Faz 10 AAD) become
// target "all" and still decrypt; old deployments get their environment.
func TestEnvironmentsMigration(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	db := rawDB(t)
	if _, err := db.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if err := st.MigrateUntil(ctx, "009_github_app"); err != nil {
		t.Fatal(err)
	}
	k := genKey(t)
	ring := keyring(t, k)
	var appID int64
	if err := db.QueryRow(`INSERT INTO apps (name, repo_full_name, production_branch, team_id)
		VALUES ('blog', 'nisagwn/blog', 'main', (SELECT id FROM teams WHERE slug = 'default')) RETURNING id`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	ct, err := ring.Encrypt([]byte("old-secret"), []byte("paas/app_env\x00"+strconv.FormatInt(appID, 10)+"\x00TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO app_env (app_id, key, value, key_id) VALUES ($1, 'TOKEN', $2, $3),
		($1, 'PLAIN', 'p', NULL)`, appID, ct, ring.CurrentID()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO deployments (app_id, commit_sha, branch) VALUES
		($1, $2, 'main'), ($1, $3, 'feature')`, appID, strings.Repeat("a", 40), strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}

	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	st.SetEnvKeyring(ring)
	for _, target := range []string{store.EnvProduction, store.EnvPreview} {
		sameEnv(t, target, resolve(t, st, appID, target, "x"), map[string]string{"TOKEN": "old-secret", "PLAIN": "p"})
	}
	vars, _ := st.ListEnv(ctx, appID)
	for _, v := range vars {
		if v.Target != store.EnvAll {
			t.Fatalf("migrated row %+v, want target all", v)
		}
	}
	// Re-encryption of the migrated rows keeps them readable.
	if n, err := st.ReencryptEnv(ctx); err != nil || n != 1 {
		t.Fatalf("encrypt plaintext row: n=%d err=%v", n, err)
	}
	sameEnv(t, "after re-encryption", resolve(t, st, appID, store.EnvPreview, "x"), map[string]string{"TOKEN": "old-secret", "PLAIN": "p"})

	deps, _ := st.ListDeployments(ctx, appID, 10)
	for _, d := range deps {
		want := store.EnvPreview
		if d.Branch == "main" {
			want = store.EnvProduction
		}
		if d.Target != want || d.Origin != store.OriginGit || d.Generation != 0 {
			t.Fatalf("migrated deployment %d (%s): target %q origin %q gen %d", d.ID, d.Branch, d.Target, d.Origin, d.Generation)
		}
	}
}

// Redeploys and promotions are new deployments of the same commit with the
// next generation; a push of the commit still finds generation 0.
func TestCopyDeployment(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	sha := strings.Repeat("c", 40)
	src, _, _ := st.EnqueueDeployment(ctx, app.ID, sha, "feature", "msg")
	if _, err := st.CopyDeployment(ctx, src, store.CopyOptions{Origin: store.OriginRedeploy, Target: store.EnvPreview}); !errors.Is(err, store.ErrInFlight) {
		t.Fatalf("copy of a queued deployment: %v", err)
	}
	src, _ = st.ClaimNext(ctx)
	st.SetImage(ctx, src.ID, "reg/blog@sha256:1")
	if err := st.MarkReady(ctx, src, nil); err != nil {
		t.Fatal(err)
	}
	src, _ = st.GetDeployment(ctx, src.ID)

	p, err := st.CopyDeployment(ctx, src, store.CopyOptions{Origin: store.OriginPromote, Target: store.EnvProduction, ReuseImage: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Generation != 1 || p.Image != "reg/blog@sha256:1" || p.Target != store.EnvProduction ||
		p.Origin != store.OriginPromote || p.SourceDeploymentID == nil || *p.SourceDeploymentID != src.ID ||
		p.Status != store.StatusQueued || p.CommitMessage != "msg" || p.Branch != "feature" {
		t.Fatalf("promoted copy: %+v", p)
	}
	if p.ObjectName() != "d-ccccccc-1" || p.Host("paas.test") != "ccccccc-1-blog.paas.test" ||
		src.ObjectName() != "d-ccccccc" || src.Host("paas.test") != "ccccccc-blog.paas.test" {
		t.Fatalf("names: %s %s / %s %s", p.ObjectName(), p.Host("paas.test"), src.ObjectName(), src.Host("paas.test"))
	}
	r, err := st.CopyDeployment(ctx, src, store.CopyOptions{Origin: store.OriginRedeploy, Target: store.EnvPreview})
	if err != nil || r.Generation != 2 || r.Image != "" {
		t.Fatalf("rebuild copy: %+v %v", r, err)
	}
	again, created, err := st.EnqueueDeployment(ctx, app.ID, sha, "feature", "")
	if err != nil || created || again.ID != src.ID {
		t.Fatalf("re-push: %+v created=%v err=%v", again, created, err)
	}
}

// Cancel: queued at once, in flight through the heartbeat, finished never.
func TestCancelDeployment(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")

	q, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("1", 40), "main", "")
	got, err := st.CancelDeployment(ctx, q.ID)
	if err != nil || got.Status != store.StatusCanceled || got.FinishedAt == nil || !got.Finished() {
		t.Fatalf("cancel queued: %+v %v", got, err)
	}
	if _, err := st.ClaimNext(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a canceled deployment was claimed: %v", err)
	}
	if _, err := st.CancelDeployment(ctx, q.ID); !errors.Is(err, store.ErrFinished) {
		t.Fatalf("cancel twice: %v", err)
	}
	if pending, _ := st.PendingCleanup(ctx, app.ID, 10); len(pending) != 0 {
		t.Fatalf("a queued cancel needs no cleanup: %+v", pending)
	}

	// Building: only the request is recorded; the heartbeat reports it.
	b, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("2", 40), "main", "")
	b, _ = st.ClaimNext(ctx)
	if hb, err := st.Beat(ctx, b.ID, b.Attempts); err != nil || !hb.Owned || hb.CancelRequested {
		t.Fatalf("beat before cancel: %+v %v", hb, err)
	}
	got, err = st.CancelDeployment(ctx, b.ID)
	if err != nil || got.Status != store.StatusBuilding || got.CancelRequestedAt == nil {
		t.Fatalf("cancel building: %+v %v", got, err)
	}
	if hb, _ := st.Beat(ctx, b.ID, b.Attempts); !hb.Owned || !hb.CancelRequested {
		t.Fatalf("beat after cancel: %+v", hb)
	}
	// The worker's failure record becomes canceled.
	st.MarkFailed(ctx, b.ID, "context canceled")
	if got, _ := st.GetDeployment(ctx, b.ID); got.Status != store.StatusCanceled || got.Error != store.CanceledReason {
		t.Fatalf("after the worker stopped: %+v", got)
	}
	if pending, _ := st.PendingCleanup(ctx, app.ID, 10); len(pending) != 1 || pending[0].ID != b.ID {
		t.Fatalf("a canceled run is cleaned up: %+v", pending)
	}

	// Deploying, and the run reaches MarkReady anyway: no aliases.
	d, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("3", 40), "main", "")
	d, _ = st.ClaimNext(ctx)
	st.SetStatus(ctx, d.ID, store.StatusDeploying)
	if got, err := st.CancelDeployment(ctx, d.ID); err != nil || got.Status != store.StatusDeploying {
		t.Fatalf("cancel deploying: %+v %v", got, err)
	}
	err = st.MarkReady(ctx, d, []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}})
	if !errors.Is(err, store.ErrCanceled) {
		t.Fatalf("MarkReady after cancel: %v", err)
	}
	if aliases, _ := st.ListAliases(ctx, app.ID); len(aliases) != 0 {
		t.Fatalf("canceled deployment got aliases: %+v", aliases)
	}

	// Ready: nothing to cancel.
	r, _, _ := st.EnqueueDeployment(ctx, app.ID, strings.Repeat("4", 40), "main", "")
	r, _ = st.ClaimNext(ctx)
	st.MarkReady(ctx, r, nil)
	if _, err := st.CancelDeployment(ctx, r.ID); !errors.Is(err, store.ErrFinished) {
		t.Fatalf("cancel ready: %v", err)
	}
	if _, err := st.CancelDeployment(ctx, 999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancel unknown: %v", err)
	}
}

func TestDeployHooksStoredHashed(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	hash := strings.Repeat("ab", 32)
	h, err := st.CreateDeployHook(ctx, app.ID, "cms", "main", hash, "paas_hook_abcdef", 0)
	if err != nil || h.Branch != "main" || h.LastTriggeredAt != nil {
		t.Fatalf("create: %+v %v", h, err)
	}
	if _, err := st.CreateDeployHook(ctx, app.ID, "dup", "main", hash, "x", 0); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate hash: %v", err)
	}
	got, err := st.TriggerDeployHook(ctx, hash)
	if err != nil || got.ID != h.ID || got.LastTriggeredAt == nil {
		t.Fatalf("trigger: %+v %v", got, err)
	}
	if _, err := st.TriggerDeployHook(ctx, strings.Repeat("cd", 32)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown hash: %v", err)
	}
	if err := st.DeleteDeployHook(ctx, app.ID+1, h.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete through another app: %v", err)
	}
	if err := st.DeleteDeployHook(ctx, app.ID, h.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListDeployHooks(ctx, app.ID); len(list) != 0 {
		t.Fatalf("after delete: %+v", list)
	}
}
