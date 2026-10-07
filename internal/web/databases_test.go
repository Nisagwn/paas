package web_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/store"
)

func TestDatabasesTab(t *testing.T) {
	u := setup(t)
	u.login()
	ctx := context.Background()
	u.post("/apps", url.Values{"name": {"blog"}, "repo": {"nisagwn/blog"}, "csrf": {u.csrf("/")}}, "self")
	app, _ := u.st.GetAppByName(ctx, "blog")

	code, body, _ := u.get("/apps/blog/databases")
	for _, want := range []string{"Veritabanları", "Henüz veritabanı yok", "Veritabanı ekle", "PostgreSQL 16", `action="/apps/blog/databases"`} {
		if code != 200 || !strings.Contains(body, want) {
			t.Fatalf("empty tab lacks %q (%d)", want, code)
		}
	}
	if _, body, _ := u.get("/apps/blog"); !strings.Contains(body, `href="/apps/blog/databases"`) {
		t.Error("no tab link")
	}
	csrf := u.csrf("/apps/blog/databases")
	post := func(path string, v url.Values) (int, string, http.Header) {
		v.Set("csrf", csrf)
		return u.post(path, v, "self")
	}

	// Validation errors come back in Turkish with the form kept.
	if code, body, _ := post("/apps/blog/databases", url.Values{"kind": {"postgres"}, "name": {"Bad_Name"}}); code != http.StatusBadRequest ||
		!strings.Contains(body, "Ad 1-20 karakter olmalı") || !strings.Contains(body, `value="Bad_Name"`) {
		t.Fatalf("bad name: %d", code)
	}
	if code, _, h := post("/apps/blog/databases", url.Values{"kind": {"postgres"}, "plan": {"hobby"}, "preview_mode": {"copy"}}); code != http.StatusSeeOther ||
		h.Get("Location") != "/apps/blog/databases?ok=db-created#db-db" {
		t.Fatalf("create: %d %s", code, h.Get("Location"))
	}
	if code, body, _ := post("/apps/blog/databases", url.Values{"kind": {"postgres"}}); code != http.StatusConflict || !strings.Contains(body, "db adlı bir eklenti zaten var") {
		t.Fatalf("duplicate: %d", code)
	}
	post("/apps/blog/databases", url.Values{"kind": {"redis"}, "preview_mode": {"copy"}}) // the preview select is ignored for Redis
	db, _ := u.st.GetAddon(ctx, app.ID, "db")
	_, body, _ = u.get("/apps/blog/databases?ok=db-created")
	for _, want := range []string{"Veritabanı hazırlanıyor", "Hazırlanıyor", "addon-db.app-blog.svc:5432", "app / app",
		"DATABASE_URL", "REDIS_URL", "Production&#39;ın kopyası", `hx-trigger="every 5s"`, "Bağlantı bilgilerini göster"} {
		if !strings.Contains(body, want) {
			t.Errorf("tab lacks %q", want)
		}
	}
	sec, _, _ := u.st.AddonSecretsOf(ctx, db)
	if strings.Contains(body, sec.Password) {
		t.Fatal("the password is on the page before it was revealed")
	}

	// Settings: rules are validated (Turkish errors) and saved.
	if code, body, _ := post("/apps/blog/databases/db/settings", url.Values{"plan": {"hobby"}, "preview_mode": {"copy"},
		"anonymize": {"users.email: shuffle"}, "backup_keep": {"7"}}); code != http.StatusBadRequest || !strings.Contains(body, "bilinmeyen strateji") {
		t.Fatalf("bad rule: %d", code)
	}
	if code, _, _ := post("/apps/blog/databases/db/settings", url.Values{"plan": {"standard"}, "preview_mode": {"empty"},
		"anonymize": {"users.email: email\r\n\r\n*.phone: null"}, "anonymize_sql": {"UPDATE t SET a = 1;"}, "backup_keep": {"5"}}); code != http.StatusSeeOther {
		t.Fatalf("settings: %d", code)
	}
	db, _ = u.st.GetAddon(ctx, app.ID, "db")
	if db.Plan != "standard" || db.PreviewMode != store.PreviewEmpty || strings.Join(db.Anonymize, "|") != "users.email: email|*.phone: null" ||
		db.AnonymizeSQL != "UPDATE t SET a = 1;" || db.BackupKeep != 5 {
		t.Fatalf("saved: %+v", db)
	}
	if code, body, _ := post("/apps/blog/databases/db/settings", url.Values{"plan": {"hobby"}}); code != http.StatusBadRequest || !strings.Contains(body, "disk daraltılamaz") {
		t.Fatalf("shrink: %d", code)
	}

	// Reveal shows the credentials once.
	code, body, _ = post("/apps/blog/databases/db/reveal", url.Values{})
	if code != 200 || !strings.Contains(body, "postgres://app:"+sec.Password+"@addon-db.app-blog.svc:5432/app?sslmode=disable") ||
		!strings.Contains(body, "kubectl -n app-blog port-forward svc/addon-db 5432:5432") {
		t.Fatalf("reveal: %d", code)
	}
	if _, body, _ := u.get("/apps/blog/databases"); strings.Contains(body, sec.Password) {
		t.Fatal("the password stayed on the page")
	}

	// Actions on a ready add-on: branch copies, backups, rotation.
	u.st.SetAddonState(ctx, db.ID, store.AddonReady, "")
	db, _ = u.st.GetAddon(ctx, app.ID, "db")
	b, _ := u.st.EnsureAddonBranch(ctx, db, "feature/x")
	u.st.StartAddonBranch(ctx, b.ID, b.Generation)
	u.st.FailAddonBranch(ctx, b.ID, b.Generation, `ERROR:  relation "users" does not exist`)
	_, body, _ = u.get("/apps/blog/databases")
	for _, want := range []string{"feature/x", b.Database, "Başarısız", "relation &#34;users&#34; does not exist", "Kopyayı yenile", "Şimdi yedekle", "Şifreyi yenile"} {
		if !strings.Contains(body, want) {
			t.Errorf("ready tab lacks %q", want)
		}
	}
	if code, _, h := post("/apps/blog/databases/db/branches/reset", url.Values{"branch": {"feature/x"}}); code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "ok=db-reset") {
		t.Fatalf("reset: %d", code)
	}
	if code, body, _ := post("/apps/blog/databases/db/branches/reset", url.Values{"branch": {"feature/x"}}); code != http.StatusConflict || !strings.Contains(body, "kopyası zaten sürüyor") {
		t.Fatalf("second reset: %d", code)
	}
	if code, _, _ := post("/apps/blog/databases/db/backups", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("backup: %d", code)
	}
	backups, _ := u.st.ListAddonBackups(ctx, db.ID, 10)
	if len(backups) != 1 || backups[0].Status != store.BackupPending {
		t.Fatalf("backups: %+v", backups)
	}
	size := int64(2048)
	u.st.RecordBackupRun(ctx, db.ID, store.BackupRun{Job: backups[0].Job, Trigger: store.BackupManual, Status: store.BackupSucceeded, SizeBytes: &size})
	restore := url.Values{"backup": {fmt.Sprint(backups[0].ID)}, "confirm": {"wrong"}}
	if code, body, _ := post("/apps/blog/databases/db/backups/restore", restore); code != http.StatusBadRequest || !strings.Contains(body, "eklentinin adını (db) yaz") {
		t.Fatalf("restore without confirmation: %d", code)
	}
	restore.Set("confirm", "db")
	if code, _, _ := post("/apps/blog/databases/db/backups/restore", restore); code != http.StatusSeeOther {
		t.Fatalf("restore: %d", code)
	}
	if b, _ := u.st.GetAddonBackup(ctx, db.ID, backups[0].ID); b.RestoreStatus != store.BackupPending || b.RestoreBy != "admin" {
		t.Fatalf("restore row: %+v", b)
	}
	if _, body, _ := u.get("/apps/blog/databases"); !strings.Contains(body, "2.0 KiB") || !strings.Contains(body, "Geri yükleme: Sırada") {
		t.Error("backup list")
	}
	if code, _, _ := post("/apps/blog/databases/db/rotate", url.Values{"redeploy": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("rotate: %d", code)
	}
	if code, body, _ := post("/apps/blog/databases/db/rotate", url.Values{}); code != http.StatusConflict || !strings.Contains(body, "Şifre yenileme zaten sürüyor") {
		t.Fatalf("second rotate: %d", code)
	}

	// Deletion needs the name.
	if code, body, _ := post("/apps/blog/databases/db/delete", url.Values{"confirm": {"x"}}); code != http.StatusBadRequest || !strings.Contains(body, "verileri ve yedekleri de siler") {
		t.Fatalf("delete without confirmation: %d", code)
	}
	if code, _, h := post("/apps/blog/databases/db/delete", url.Values{"confirm": {"db"}}); code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "ok=db-deleted") {
		t.Fatalf("delete: %d", code)
	}
	if db, _ = u.st.GetAddon(ctx, app.ID, "db"); db.Status != store.AddonDeleting {
		t.Fatalf("status %s", db.Status)
	}
	if code, _, _ := post("/apps/blog/databases/nope/reveal", url.Values{}); code != http.StatusNotFound {
		t.Errorf("unknown add-on: %d", code)
	}
}

func TestDatabasesTabViewer(t *testing.T) {
	u := setup(t)
	ctx := context.Background()
	st := u.st
	alice, _ := st.UpsertUser(ctx, 1, "alice", "", "")
	carol, _ := st.UpsertUser(ctx, 2, "carol", "", "")
	team, _ := st.CreateTeam(ctx, "web", "Web", alice.ID)
	st.SetMember(ctx, team.ID, carol.ID, store.RoleViewer)
	app, _ := st.CreateAppInTeam(ctx, team.ID, "blog", "nisagwn/blog", "main")
	st.CreateAddon(ctx, app.ID, store.NewAddon{Kind: "postgres", Name: "db", Plan: "hobby", PreviewMode: "copy"})

	viewer := u.as(carol.ID)
	code, body, _ := viewer.get("/apps/blog/databases")
	if code != 200 || !strings.Contains(body, "addon-db.app-blog.svc") {
		t.Fatalf("viewer tab: %d", code)
	}
	for _, hidden := range []string{"Bağlantı bilgilerini göster", "Veritabanı ekle", "/databases/db/delete", "Şifreyi yenile"} {
		if strings.Contains(body, hidden) {
			t.Errorf("viewer sees %q", hidden)
		}
	}
	csrf := viewer.csrf("/apps/blog/databases")
	for _, path := range []string{"/apps/blog/databases", "/apps/blog/databases/db/reveal", "/apps/blog/databases/db/delete"} {
		if code, _, _ := viewer.post(path, url.Values{"csrf": {csrf}, "kind": {"redis"}, "confirm": {"db"}}, "self"); code != http.StatusForbidden {
			t.Errorf("viewer POST %s: %d", path, code)
		}
	}
}
