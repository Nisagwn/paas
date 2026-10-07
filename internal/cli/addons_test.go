package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const addonBody = `{"id":3,"kind":"postgres","name":"db","plan":"hobby","status":"ready","preview_mode":"copy",
 "anonymize":["users.email: email"],"anonymize_sql":"","backup_keep":7,"rotating":false,
 "size":{"id":"hobby","memory":"256Mi","storage":"1Gi"},"host":"addon-db.app-blog.svc","port":5432,
 "user":"app","database":"app","variables":["DATABASE_URL","PGDATABASE","PGHOST","PGPASSWORD","PGPORT","PGUSER"]}`

const credsBody = `{"addon":"db","kind":"postgres","host":"addon-db.app-blog.svc","port":5432,"user":"app",
 "password":"s3cret","database":"app","url":"postgres://app:s3cret@addon-db.app-blog.svc:5432/app?sslmode=disable"}`

func TestAddonsLsAddInfo(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.json("GET /api/apps/blog/addons", 200, `[`+addonBody+`,{"id":4,"kind":"redis","name":"cache","plan":"hobby",
		"status":"provisioning","preview_mode":"shared","variables":["REDIS_URL"]}]`)
	res := run(t, loggedIn(f), "", "addons", "ls", "blog")
	for _, want := range []string{"NAME   KIND      PLAN   STATUS", "db     postgres  hobby  ready", "cache  redis     hobby  provisioning  shared", "REDIS_URL"} {
		if res.code != ExitOK || !strings.Contains(res.stdout, want) {
			t.Errorf("ls lacks %q: %d\n%s%s", want, res.code, res.stdout, res.stderr)
		}
	}

	f.json("POST /api/apps/blog/addons", 201, addonBody)
	res = run(t, loggedIn(f), "", "addons", "add", "blog", "postgres", "--plan", "hobby", "--preview-mode", "copy")
	if res.code != ExitOK || !strings.Contains(res.stdout, "Created postgres add-on db on blog (hobby, 256Mi memory, 1Gi disk)") {
		t.Fatalf("add: %d %q %q", res.code, res.stdout, res.stderr)
	}
	if got := f.body("POST /api/apps/blog/addons"); len(got) != 1 || got[0] != `{"kind":"postgres","name":"","plan":"hobby","preview_mode":"copy"}` {
		t.Errorf("add body = %q", got)
	}

	f.json("GET /api/apps/blog/addons/db", 200, addonBody)
	res = run(t, loggedIn(f), "", "addons", "info", "blog", "db")
	if res.code != ExitOK || !strings.Contains(res.stdout, "addon-db.app-blog.svc:5432") || strings.Contains(res.stdout, "s3cret") ||
		!strings.Contains(res.stdout, "users.email: email") {
		t.Fatalf("info: %q", res.stdout)
	}
	f.json("POST /api/apps/blog/addons/db/credentials/reveal", 200, credsBody)
	res = run(t, loggedIn(f), "", "addons", "info", "blog", "db", "--reveal")
	if res.code != ExitOK || !strings.Contains(res.stdout, "Password (production):  s3cret") || !strings.Contains(res.stderr, "logged") {
		t.Fatalf("info --reveal: %q %q", res.stdout, res.stderr)
	}
	run(t, loggedIn(f), "", "addons", "info", "blog", "db", "--reveal", "--branch", "feature/x")
	if got := f.body("POST /api/apps/blog/addons/db/credentials/reveal"); got[len(got)-1] != `{"branch":"feature/x"}` {
		t.Errorf("branch reveal body = %q", got)
	}
}

func TestAddonsChanges(t *testing.T) {
	dir := isolate(t)
	f := newFakeAPI(t)
	f.json("PATCH /api/apps/blog/addons/db", 200, addonBody)
	sqlFile := filepath.Join(dir, "anon.sql")
	os.WriteFile(sqlFile, []byte("UPDATE orders SET note = NULL;"), 0o600)
	res := run(t, loggedIn(f), "", "addons", "set", "blog", "db", "--anonymize", "users.email: email", "--anonymize", "*.phone: null",
		"--anonymize-sql-file", sqlFile, "--backup-keep", "0", "--preview-mode", "empty")
	if res.code != ExitOK {
		t.Fatalf("set: %d %q", res.code, res.stderr)
	}
	if got := f.body("PATCH /api/apps/blog/addons/db"); got[0] != `{"anonymize":["users.email: email","*.phone: null"],"anonymize_sql":"UPDATE orders SET note = NULL;","backup_keep":0,"preview_mode":"empty"}` {
		t.Errorf("set body = %q", got)
	}
	run(t, loggedIn(f), "", "addons", "set", "blog", "db", "--clear-anonymize", "--anonymize-sql-file", "-")
	if got := f.body("PATCH /api/apps/blog/addons/db"); got[1] != `{"anonymize":[],"anonymize_sql":""}` {
		t.Errorf("clear body = %q", got)
	}

	f.json("DELETE /api/apps/blog/addons/db", 202, addonBody)
	if res := run(t, loggedIn(f), "", "addons", "rm", "blog", "db"); res.code != ExitUsage || !strings.Contains(res.stderr, "--confirm db") {
		t.Errorf("rm without confirm: %d %q", res.code, res.stderr)
	}
	if res := run(t, loggedIn(f), "", "addons", "rm", "blog", "db", "--confirm", "db"); res.code != ExitOK || !strings.Contains(res.stdout, "Deleting db of blog") {
		t.Errorf("rm: %d %q", res.code, res.stderr)
	}

	f.json("POST /api/apps/blog/addons/db/rotate", 202, `{"addon":"db","rotating":true,"redeploy":false,"note":"the new password is applied within a minute"}`)
	run(t, loggedIn(f), "", "addons", "rotate", "blog", "db", "--no-redeploy")
	if got := f.body("POST /api/apps/blog/addons/db/rotate"); got[0] != `{"redeploy":false}` {
		t.Errorf("rotate body = %q", got)
	}

	var resetPath string
	f.mux.HandleFunc("POST /api/apps/blog/addons/db/branches/{branch}/reset", func(w http.ResponseWriter, r *http.Request) {
		resetPath = r.URL.EscapedPath()
		w.WriteHeader(202)
		w.Write([]byte(`{}`))
	})
	if res := run(t, loggedIn(f), "", "addons", "reset", "blog", "db", "feature/x"); res.code != ExitOK ||
		resetPath != "/api/apps/blog/addons/db/branches/feature%2Fx/reset" {
		t.Errorf("reset: %d %q %q", res.code, resetPath, res.stderr)
	}

	f.json("GET /api/apps/blog/addons/db/branches", 200, `[{"branch":"feature/x","database":"preview_feature_x_1a2b3c4d","mode":"empty",
		"status":"ready","warning":"production is 6.0 GiB, above the copy limit of 5.0 GiB: an empty database was created instead",
		"snapshot_at":"2026-10-05T11:00:00Z","size_bytes":7765015}]`)
	res = run(t, loggedIn(f), "", "addons", "branches", "blog", "db")
	if res.code != ExitOK || !strings.Contains(res.stdout, "feature/x  preview_feature_x_1a2b3c4d  empty  ready   1h ago    7.4 MiB") ||
		!strings.Contains(res.stdout, "above the copy limit") {
		t.Errorf("branches: %q", res.stdout)
	}

	f.json("POST /api/apps/blog/addons/db/backups", 202, `{"id":9,"job":"addon-db-backup-m9","status":"pending"}`)
	if res := run(t, loggedIn(f), "", "addons", "backup", "blog", "db"); !strings.Contains(res.stdout, "Backup #9 of db started") {
		t.Errorf("backup: %q", res.stdout)
	}
	f.json("GET /api/apps/blog/addons/db/backups", 200, `[{"id":9,"job":"addon-db-backup-m9","trigger":"manual","status":"succeeded",
		"size_bytes":35034,"finished_at":"2026-10-05T11:30:00Z","restore_status":"failed","restore_error":"backup file is no longer on the backup volume",
		"created_at":"2026-10-05T11:29:00Z"}]`)
	if res := run(t, loggedIn(f), "", "addons", "backups", "blog", "db"); !strings.Contains(res.stdout, "9   30m ago  manual   succeeded  34.2 KiB  failed: backup file") {
		t.Errorf("backups: %q", res.stdout)
	}
	f.json("POST /api/apps/blog/addons/db/backups/9/restore", 202, `{}`)
	if res := run(t, loggedIn(f), "", "addons", "restore", "blog", "db", "9"); res.code != ExitUsage {
		t.Errorf("restore without confirm: %d", res.code)
	}
	if res := run(t, loggedIn(f), "", "addons", "restore", "blog", "db", "#9", "--confirm", "db"); res.code != ExitOK ||
		f.body("POST /api/apps/blog/addons/db/backups/9/restore")[0] != `{"confirm":"db"}` {
		t.Errorf("restore: %d %q", res.code, res.stderr)
	}

	for _, args := range [][]string{
		{"addons", "explode", "blog"},
		{"addons", "info", "blog"},
		{"addons", "reset", "blog", "db"},
		{"addons", "set", "blog", "db"},
		{"addons", "restore", "blog", "db", "x", "--confirm", "db"},
		{"db", "mysql", "blog"},
	} {
		if res := run(t, loggedIn(f), "", args...); res.code != ExitUsage {
			t.Errorf("%v: code %d, want usage error", args, res.code)
		}
	}
}

func TestDBPsql(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.json("GET /api/apps/blog/addons", 200, `[{"name":"cache","kind":"redis"},{"name":"analytics","kind":"postgres"},{"name":"db","kind":"postgres"}]`)
	f.json("POST /api/apps/blog/addons/db/credentials/reveal", 200, credsBody)
	res := run(t, loggedIn(f), "", "db", "psql", "blog")
	for _, want := range []string{
		"kubectl -n app-blog port-forward svc/addon-db 15432:5432",
		`psql "postgres://app:s3cret@localhost:15432/app?sslmode=disable"`,
		"the platform has no tunnel of its own",
	} {
		if res.code != ExitOK || !strings.Contains(res.stdout, want) {
			t.Errorf("psql lacks %q: %d\n%s%s", want, res.code, res.stdout, res.stderr)
		}
	}
	f.json("GET /api/apps/empty/addons", 200, `[]`)
	if res := run(t, loggedIn(f), "", "db", "psql", "empty"); res.code != ExitError || !strings.Contains(res.stderr, "has no Postgres add-on") {
		t.Errorf("no add-on: %d %q", res.code, res.stderr)
	}
}
