package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/nisagwn/paas/internal/addons"
	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 22: the "Veritabanları" tab (templates/databases.html). Members add
// a Postgres or Redis add-on, see its connection details (the password
// only after "Göster", which is logged), choose what previews connect to,
// write anonymization rules, refresh a branch's copy, back up and restore,
// rotate the password and delete it (typing its name). Every action goes
// through the same helpers as the JSON API (api.CreateAddon, ...).

var addonStatusLabels = map[string]string{
	store.AddonProvisioning: "Hazırlanıyor",
	store.AddonReady:        "Hazır",
	store.AddonFailed:       "Başarısız",
	store.AddonDeleting:     "Siliniyor",
	store.BranchPending:     "Sırada",
	store.BranchCopying:     "Kopyalanıyor",
	store.BackupRunning:     "Sürüyor",
	store.BackupSucceeded:   "Başarılı",
	store.BackupExpired:     "Saklama süresi doldu",
}

var addonStatusClasses = map[string]string{
	store.AddonProvisioning: "s-building",
	store.AddonReady:        "s-ready",
	store.AddonFailed:       "s-failed",
	store.AddonDeleting:     "s-canceled",
	store.BranchPending:     "s-queued",
	store.BranchCopying:     "s-building",
	store.BackupRunning:     "s-building",
	store.BackupSucceeded:   "s-ready",
	store.BackupExpired:     "s-retired",
}

var previewModeLabels = map[string]string{
	store.PreviewCopy:   "Production'ın kopyası",
	store.PreviewEmpty:  "Boş veritabanı",
	store.PreviewShared: "Production'ı paylaş",
}

var addonKindLabels = map[string]string{store.AddonPostgres: "PostgreSQL", store.AddonRedis: "Redis"}

// addonMessages translate the messages of the api add-on helpers.
var addonMessages = map[string]string{
	`kind must be "postgres" or "redis"`:                                                      "Tür PostgreSQL ya da Redis olmalı.",
	"name must be 1-20 characters: lowercase letters, digits and '-', starting with a letter": "Ad 1-20 karakter olmalı: a-z harfleri, rakamlar ve '-'; harfle başlamalı.",
	`preview_mode must be "copy", "empty" or "shared"`:                                        "Önizleme modu kopya, boş ya da paylaşımlı olmalı.",
	`a Redis add-on is shared by previews: preview_mode must be "shared"`:                     "Redis önizlemelerle paylaşılır; önizleme modu \"Production'ı paylaş\" olmalı.",
	"invalid add-on":                                          "Geçersiz eklenti.",
	"invalid add-on settings":                                 "Geçersiz eklenti ayarı.",
	"the add-on is being deleted":                             "Eklenti siliniyor.",
	"the add-on is not ready":                                 "Eklenti henüz hazır değil.",
	"anonymization rules apply to Postgres add-ons only":      "Anonimleştirme kuralları yalnızca PostgreSQL için geçerlidir.",
	"anonymization statements apply to Postgres add-ons only": "Anonimleştirme SQL'i yalnızca PostgreSQL için geçerlidir.",
	"backups apply to Postgres add-ons only":                  "Yedekler yalnızca PostgreSQL için alınır.",
	"a password rotation is already running":                  "Şifre yenileme zaten sürüyor.",
	"previews share the production database (preview_mode shared): there is no copy to refresh":     "Önizlemeler production veritabanını paylaşıyor; yenilenecek bir kopya yok.",
	"a copy of this branch is already running":                                                      "Bu branch'in kopyası zaten sürüyor.",
	"a backup is already running":                                                                   "Bir yedekleme zaten sürüyor.",
	"backup not found":                                                                              "Yedek bulunamadı.",
	"only a succeeded backup that was not pruned can be restored":                                   "Yalnızca başarılı ve hâlâ saklanan bir yedek geri yüklenebilir.",
	"a restore is already running":                                                                  "Bir geri yükleme zaten sürüyor.",
	`anonymize_sql must be plain SQL: psql meta-commands (lines starting with "\") are not allowed`: `Anonimleştirme SQL'i düz SQL olmalı: "\" ile başlayan psql komutlarına izin verilmez.`,
}

var addonPatterns = []struct {
	re *regexp.Regexp
	tr string
}{
	{regexp.MustCompile(`^plan must be one of (.+)$`), "Boyut şunlardan biri olmalı: %s."},
	{regexp.MustCompile(`^the app already has an add-on named "(.+)"$`), "Bu projede %s adlı bir eklenti zaten var."},
	{regexp.MustCompile(`^an app can have at most (\d+) add-ons$`), "Bir projede en fazla %s eklenti olabilir."},
	{regexp.MustCompile(`^plan (\w+) has a smaller volume \((.+)\) than (\w+) \((.+)\): volumes cannot shrink$`), "%s boyutunun diski (%s), %s boyutununkinden (%s) dar: disk daraltılamaz."},
	{regexp.MustCompile(`^backup_keep must be between 0 \(no daily backups\) and (\d+)$`), "Saklanacak yedek sayısı 0 (günlük yedek yok) ile %s arasında olmalı."},
	{regexp.MustCompile(`^deleting an add-on deletes its data and backups: confirm must be the add-on's name \((.+)\)$`), "Silme işlemi verileri ve yedekleri de siler; onaylamak için eklentinin adını (%s) yaz."},
	{regexp.MustCompile(`^restoring replaces the production database: confirm must be the add-on's name \((.+)\)$`), "Geri yükleme production veritabanının yerini alır; onaylamak için eklentinin adını (%s) yaz."},
	{regexp.MustCompile(`^the add-on has no database for branch "(.+)"$`), "Bu eklentide %s branch'i için bir veritabanı yok."},
	{regexp.MustCompile(`^rule "(.*)": write it as table\.column: strategy \(e\.g\. users\.email: email\)$`), "Kural %q: tablo.sütun: strateji biçiminde yaz (ör. users.email: email)."},
	{regexp.MustCompile(`^rule "(.*)": unknown strategy "(.*)" \(one of (.+)\)$`), "Kural %q: bilinmeyen strateji %q (şunlardan biri: %s)."},
	{regexp.MustCompile(`^rule "(.*)": the column must be table\.column or schema\.table\.column$`), "Kural %q: sütun tablo.sütun ya da şema.tablo.sütun biçiminde olmalı."},
	{regexp.MustCompile(`^rule "(.*)": a wildcard rule is \*\.column$`), "Kural %q: joker kural *.sütun biçimindedir."},
	{regexp.MustCompile(`^rule "(.*)": table names must be lower-case identifiers \(a-z, 0-9, _\)$`), "Kural %q: tablo adları a-z, 0-9 ve _ içerebilir."},
	{regexp.MustCompile(`^rule "(.*)": column names must be lower-case identifiers \(a-z, 0-9, _\)$`), "Kural %q: sütun adları a-z, 0-9 ve _ içerebilir."},
	{regexp.MustCompile(`^rule "(.*)": system schemas cannot be anonymized$`), "Kural %q: sistem şemaları anonimleştirilemez."},
	{regexp.MustCompile(`^rule "(.*)": the column has another rule already$`), "Kural %q: bu sütun için zaten bir kural var."},
	{regexp.MustCompile(`^at most (\d+) anonymization rules$`), "En fazla %s anonimleştirme kuralı yazılabilir."},
	{regexp.MustCompile(`^anonymize_sql is longer than (\d+) bytes$`), "Anonimleştirme SQL'i en fazla %s bayt olabilir."},
}

func init() {
	funcs["dbstate"] = label(addonStatusLabels)
	funcs["dbclass"] = func(s string) string { return addonStatusClasses[s] }
	funcs["previewmode"] = label(previewModeLabels)
	funcs["dbkind"] = label(addonKindLabels)
	funcs["humanbytes"] = func(n *int64) string {
		if n == nil {
			return "—"
		}
		return addons.HumanBytes(*n)
	}
	funcs["warningtr"] = addons.WarningTR
	funcs["lines"] = func(s []string) string { return strings.Join(s, "\n") }
	for k, v := range addonMessages {
		normMessages[norm(k)] = v
	}
	patterns = append(patterns, addonPatterns...)
}

// addonPanel is one add-on on the tab.
type addonPanel struct {
	api.AddonView
	Branches []store.AddonBranch
	Backups  []store.AddonBackup
	// Revealed holds the credentials after "Göster" (only in that response).
	Revealed *api.AddonCredentials
}

// databasesData is the data of the tab.
type databasesData struct {
	Addons []addonPanel
	Plans  []store.AddonPlan
	// Busy: something is being provisioned or copied; the status blocks poll.
	Busy bool
	// Form keeps the create form's values after an error.
	Form map[string]string
	// Strategies of the anonymization rules, for the help text.
	Strategies string
}

func (s *Server) databasesData(ctx context.Context, app store.App) (*databasesData, error) {
	views, err := api.AddonViews(ctx, s.Store, app)
	if err != nil {
		return nil, err
	}
	d := &databasesData{Plans: store.AddonPlans, Strategies: strings.Join(addons.Strategies, ", "),
		Form: map[string]string{"Kind": store.AddonPostgres}}
	for _, v := range views {
		p := addonPanel{AddonView: v}
		if v.Status == store.AddonProvisioning || v.Status == store.AddonDeleting || v.Rotating {
			d.Busy = true
		}
		if v.Kind == store.AddonPostgres {
			if p.Branches, err = s.Store.ListAddonBranches(ctx, v.ID); err != nil {
				return nil, err
			}
			if p.Backups, err = s.Store.ListAddonBackups(ctx, v.ID, 20); err != nil {
				return nil, err
			}
			for _, b := range p.Branches {
				if b.Status == store.BranchPending || b.Status == store.BranchCopying || b.Status == store.BranchDeleting {
					d.Busy = true
				}
			}
			for _, b := range p.Backups {
				if b.Status == store.BackupPending || b.Status == store.BackupRunning ||
					b.RestoreStatus == store.BackupPending || b.RestoreStatus == store.BackupRunning {
					d.Busy = true
				}
			}
		}
		d.Addons = append(d.Addons, p)
	}
	return d, nil
}

func (s *Server) databasesPage(w http.ResponseWriter, r *http.Request) {
	s.renderTab(w, r, http.StatusOK, tabDatabases, "", nil)
}

// dbResult ends an action: errors re-render the tab, success redirects
// back with a flash.
func (s *Server) dbResult(w http.ResponseWriter, r *http.Request, app store.App, err error, flash, anchor string) {
	var ae *api.AddonError
	switch {
	case errors.As(err, &ae):
		s.renderTabOf(w, r, app, ae.Status, tabDatabases, ae.Msg, nil)
	case err != nil:
		s.internalError(w, r, err)
	default:
		redirect(w, r, "/apps/"+app.Name+"/databases?ok="+flash+anchor)
	}
}

// formAddon loads the {addon} add-on of the app for a form.
func (s *Server) formAddon(w http.ResponseWriter, r *http.Request) (store.App, store.Addon, bool) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return app, store.Addon{}, false
	}
	a, err := s.Store.GetAddon(r.Context(), app.ID, r.PathValue("addon"))
	if errors.Is(err, store.ErrNotFound) {
		s.renderTabOf(w, r, app, http.StatusNotFound, tabDatabases, "Eklenti bulunamadı; sayfa güncel olmayabilir.", nil)
		return app, a, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return app, a, false
	}
	return app, a, true
}

func by(r *http.Request) string {
	me, _ := auth.From(r.Context())
	return me.Login
}

func (s *Server) createDatabase(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	req := api.CreateAddonRequest{
		Kind: r.PostFormValue("kind"), Name: strings.TrimSpace(r.PostFormValue("name")),
		Plan: r.PostFormValue("plan"), PreviewMode: r.PostFormValue("preview_mode"),
	}
	if req.Kind == store.AddonRedis {
		req.PreviewMode = "" // the form's preview select applies to Postgres
	}
	a, err := api.CreateAddon(r.Context(), s.Store, app, req)
	var ae *api.AddonError
	if errors.As(err, &ae) {
		s.renderTabOf(w, r, app, ae.Status, tabDatabases, ae.Msg, func(v *appDetail) {
			v.DB.Form = map[string]string{"Kind": req.Kind, "Name": req.Name, "Plan": req.Plan, "PreviewMode": req.PreviewMode, "Open": "1"}
		})
		return
	}
	if err == nil {
		s.Log.Info("addon created", "app", app.Name, "addon", a.Name, "kind", a.Kind, "plan", a.Plan, "by", by(r), "via", "web")
	}
	s.dbResult(w, r, app, err, "db-created", "#db-"+a.Name)
}

func (s *Server) updateDatabase(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.formAddon(w, r)
	if !ok {
		return
	}
	var req api.UpdateAddonRequest
	if v := r.PostFormValue("plan"); v != "" {
		req.Plan = &v
	}
	if v := r.PostFormValue("preview_mode"); v != "" {
		req.PreviewMode = &v
	}
	if a.Kind == store.AddonPostgres {
		rules := strings.Split(strings.ReplaceAll(r.PostFormValue("anonymize"), "\r\n", "\n"), "\n")
		sql := r.PostFormValue("anonymize_sql")
		req.Anonymize, req.AnonymizeSQL = &rules, &sql
		if v := strings.TrimSpace(r.PostFormValue("backup_keep")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				s.renderTabOf(w, r, app, http.StatusBadRequest, tabDatabases, "Saklanacak yedek sayısı bir sayı olmalı.", nil)
				return
			}
			req.BackupKeep = &n
		}
	}
	_, err := api.UpdateAddon(r.Context(), s.Store, a, req)
	if err == nil {
		s.Log.Info("addon updated", "app", app.Name, "addon", a.Name, "by", by(r), "via", "web")
	}
	s.dbResult(w, r, app, err, "db-saved", "#db-"+a.Name)
}

// revealDatabase renders the tab with the add-on's credentials shown
// once; the reveal is logged like the API's.
func (s *Server) revealDatabase(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.formAddon(w, r)
	if !ok {
		return
	}
	branch := strings.TrimSpace(r.PostFormValue("branch"))
	creds, err := api.RevealAddon(r.Context(), s.Store, a, branch)
	if err != nil {
		s.dbResult(w, r, app, err, "", "")
		return
	}
	s.Log.Info("addon credentials revealed", "app", app.Name, "addon", a.Name, "branch", branch, "by", by(r), "via", "web")
	s.renderTabOf(w, r, app, http.StatusOK, tabDatabases, "", func(v *appDetail) {
		for i := range v.DB.Addons {
			if v.DB.Addons[i].ID == a.ID {
				v.DB.Addons[i].Revealed = &creds
			}
		}
	})
}

func (s *Server) rotateDatabase(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.formAddon(w, r)
	if !ok {
		return
	}
	redeploy := r.PostFormValue("redeploy") != ""
	_, err := api.RotateAddon(r.Context(), s.Store, a, redeploy)
	if err == nil {
		s.Log.Info("addon rotation requested", "app", app.Name, "addon", a.Name, "redeploy", redeploy, "by", by(r), "via", "web")
	}
	s.dbResult(w, r, app, err, "db-rotate", "#db-"+a.Name)
}

func (s *Server) resetDatabaseBranch(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.formAddon(w, r)
	if !ok {
		return
	}
	branch := r.PostFormValue("branch")
	_, err := api.ResetBranch(r.Context(), s.Store, a, branch)
	if err == nil {
		s.Log.Info("addon branch reset", "app", app.Name, "addon", a.Name, "branch", branch, "by", by(r), "via", "web")
	}
	s.dbResult(w, r, app, err, "db-reset", "#db-"+a.Name)
}

func (s *Server) backupDatabase(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.formAddon(w, r)
	if !ok {
		return
	}
	b, err := api.CreateBackup(r.Context(), s.Store, a)
	if err == nil {
		s.Log.Info("addon backup requested", "app", app.Name, "addon", a.Name, "backup", b.ID, "by", by(r), "via", "web")
	}
	s.dbResult(w, r, app, err, "db-backup", "#db-"+a.Name)
}

func (s *Server) restoreDatabase(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.formAddon(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("backup"), 10, 64)
	if err != nil || id <= 0 {
		s.renderTabOf(w, r, app, http.StatusBadRequest, tabDatabases, "Geçersiz yedek.", nil)
		return
	}
	_, err = api.RestoreBackup(r.Context(), s.Store, a, id, strings.TrimSpace(r.PostFormValue("confirm")), by(r))
	if err == nil {
		s.Log.Warn("addon restore requested", "app", app.Name, "addon", a.Name, "backup", id, "by", by(r), "via", "web")
	}
	s.dbResult(w, r, app, err, "db-restore", "#db-"+a.Name)
}

func (s *Server) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.formAddon(w, r)
	if !ok {
		return
	}
	_, err := api.DeleteAddon(r.Context(), s.Store, a, strings.TrimSpace(r.PostFormValue("confirm")))
	if err == nil {
		s.Log.Info("addon deletion started", "app", app.Name, "addon", a.Name, "by", by(r), "via", "web")
	}
	s.dbResult(w, r, app, err, "db-deleted", "")
}
