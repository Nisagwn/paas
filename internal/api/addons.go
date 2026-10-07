package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/nisagwn/paas/internal/addons"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 22: managed databases (add-ons).
//
//	GET    /api/apps/{name}/addons                                   viewer
//	POST   /api/apps/{name}/addons                                   member  {"kind","name"?,"plan"?,"preview_mode"?}
//	GET    /api/apps/{name}/addons/{addon}                           viewer
//	PATCH  /api/apps/{name}/addons/{addon}                           member  {"plan"?,"preview_mode"?,"anonymize"?,"anonymize_sql"?,"backup_keep"?}
//	DELETE /api/apps/{name}/addons/{addon}?confirm=<addon>           member  (volumes are deleted)
//	POST   /api/apps/{name}/addons/{addon}/credentials/reveal        member  {"branch"?}; logged
//	POST   /api/apps/{name}/addons/{addon}/rotate                    member  {"redeploy"?: true}
//	GET    /api/apps/{name}/addons/{addon}/branches                  viewer
//	POST   /api/apps/{name}/addons/{addon}/branches/{branch}/reset   member  (branch URL-escaped: feature%2Fx)
//	GET    /api/apps/{name}/addons/{addon}/backups                   viewer
//	POST   /api/apps/{name}/addons/{addon}/backups                   member
//	POST   /api/apps/{name}/addons/{addon}/backups/{id}/restore      member  {"confirm": "<addon>"}
//
// Passwords leave the platform only through reveal (member; every reveal
// is logged with the caller) and the deployments' variables. The helpers
// (CreateAddon, UpdateAddon, ...) are shared with the web UI; their error
// messages are English and translated there.

// addonNameRe mirrors the CHECK constraint on addons.name.
var addonNameRe = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,18}[a-z0-9])?$`)

// AddonView is an add-on as the API shows it: no passwords.
type AddonView struct {
	store.Addon
	Size     store.AddonPlan `json:"size"`
	Host     string          `json:"host"`
	Port     int             `json:"port"`
	User     string          `json:"user,omitempty"`
	Database string          `json:"database,omitempty"`
	// Variables the add-on gives deployments (values only through reveal).
	Variables []string `json:"variables"`
}

// AddonViews lists the app's add-ons.
func AddonViews(ctx context.Context, st *store.Store, app store.App) ([]AddonView, error) {
	list, err := st.ListAddons(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	// Which add-on sets the plain names once they are all ready (the
	// variables shown are what the add-on gives, not only what it gives now).
	live := make([]store.Addon, 0, len(list))
	for _, a := range list {
		if a.Status != store.AddonDeleting {
			a.Status = store.AddonReady
			live = append(live, a)
		}
	}
	primary := store.PrimaryAddons(live)
	out := make([]AddonView, 0, len(list))
	for _, a := range list {
		out = append(out, addonView(a, primary[a.Kind] == a.ID))
	}
	return out, nil
}

func addonView(a store.Addon, plain bool) AddonView {
	plan, _ := store.GetAddonPlan(a.Plan)
	v := AddonView{Addon: a, Size: plan, Host: a.Host(), Port: a.Port(), Variables: store.AddonVarNames(a, plain)}
	if a.Kind == store.AddonPostgres {
		v.User, v.Database = store.PostgresAppUser, store.PostgresAppDatabase
	}
	return v
}

// AddonViewOf returns one add-on's view.
func AddonViewOf(ctx context.Context, st *store.Store, app store.App, a store.Addon) (AddonView, error) {
	views, err := AddonViews(ctx, st, app)
	if err != nil {
		return AddonView{}, err
	}
	for _, v := range views {
		if v.ID == a.ID {
			return v, nil
		}
	}
	return addonView(a, false), nil
}

// AddonError is a refused add-on request: Status is the HTTP status, Msg
// the English message.
type AddonError struct {
	Status int
	Msg    string
}

func (e *AddonError) Error() string { return e.Msg }

func badAddon(format string, args ...any) error {
	return &AddonError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// CreateAddonRequest is the body of POST /addons.
type CreateAddonRequest struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Plan        string `json:"plan"`
	PreviewMode string `json:"preview_mode"`
}

// CreateAddon validates req and stores a new add-on; the reconcile loop
// provisions it.
func CreateAddon(ctx context.Context, st *store.Store, app store.App, req CreateAddonRequest) (store.Addon, error) {
	req.Kind, req.Name = strings.ToLower(strings.TrimSpace(req.Kind)), strings.TrimSpace(req.Name)
	if !store.ValidAddonKind(req.Kind) {
		return store.Addon{}, badAddon(`kind must be "postgres" or "redis"`)
	}
	if req.Name == "" {
		req.Name = store.DefaultAddonName(req.Kind)
	}
	if !addonNameRe.MatchString(req.Name) {
		return store.Addon{}, badAddon("name must be 1-20 characters: lowercase letters, digits and '-', starting with a letter")
	}
	if req.Plan == "" {
		req.Plan = store.DefaultAddonPlan
	}
	if _, ok := store.GetAddonPlan(req.Plan); !ok {
		return store.Addon{}, badAddon("plan must be one of %s", planIDs())
	}
	if req.PreviewMode == "" {
		req.PreviewMode = store.PreviewCopy
		if req.Kind == store.AddonRedis {
			req.PreviewMode = store.PreviewShared
		}
	}
	if err := checkPreviewMode(req.Kind, req.PreviewMode); err != nil {
		return store.Addon{}, err
	}
	a, err := st.CreateAddon(ctx, app.ID, store.NewAddon{Kind: req.Kind, Name: req.Name, Plan: req.Plan, PreviewMode: req.PreviewMode})
	switch {
	case errors.Is(err, store.ErrConflict):
		return a, &AddonError{http.StatusConflict, fmt.Sprintf("the app already has an add-on named %q", req.Name)}
	case errors.Is(err, store.ErrAddonLimit):
		return a, &AddonError{http.StatusConflict, err.Error()}
	case errors.Is(err, store.ErrInvalid):
		return a, badAddon("invalid add-on")
	}
	return a, err
}

func planIDs() string {
	ids := make([]string, len(store.AddonPlans))
	for i, p := range store.AddonPlans {
		ids[i] = p.ID
	}
	return strings.Join(ids, ", ")
}

func checkPreviewMode(kind, mode string) error {
	if !store.ValidPreviewMode(mode) {
		return badAddon(`preview_mode must be "copy", "empty" or "shared"`)
	}
	if kind == store.AddonRedis && mode != store.PreviewShared {
		return badAddon(`a Redis add-on is shared by previews: preview_mode must be "shared"`)
	}
	return nil
}

// UpdateAddonRequest is the body of PATCH /addons/{addon}; omitted fields
// keep their value.
type UpdateAddonRequest struct {
	Plan         *string   `json:"plan"`
	PreviewMode  *string   `json:"preview_mode"`
	Anonymize    *[]string `json:"anonymize"`
	AnonymizeSQL *string   `json:"anonymize_sql"`
	BackupKeep   *int      `json:"backup_keep"`
}

// UpdateAddon validates req and applies it.
func UpdateAddon(ctx context.Context, st *store.Store, a store.Addon, req UpdateAddonRequest) (store.Addon, error) {
	if a.Status == store.AddonDeleting {
		return a, &AddonError{http.StatusConflict, "the add-on is being deleted"}
	}
	var c store.AddonChange
	if req.Plan != nil && *req.Plan != a.Plan {
		next, ok := store.GetAddonPlan(*req.Plan)
		if !ok {
			return a, badAddon("plan must be one of %s", planIDs())
		}
		cur, _ := store.GetAddonPlan(a.Plan)
		if q, have := resource.MustParse(next.Storage), resource.MustParse(cur.Storage); q.Cmp(have) < 0 {
			return a, badAddon("plan %s has a smaller volume (%s) than %s (%s): volumes cannot shrink", next.ID, next.Storage, cur.ID, cur.Storage)
		}
		c.Plan = req.Plan
	}
	if req.PreviewMode != nil {
		if err := checkPreviewMode(a.Kind, *req.PreviewMode); err != nil {
			return a, err
		}
		c.PreviewMode = req.PreviewMode
	}
	postgresOnly := a.Kind == store.AddonPostgres
	if req.Anonymize != nil {
		if !postgresOnly {
			return a, badAddon("anonymization rules apply to Postgres add-ons only")
		}
		rules, err := addons.NormalizeRules(*req.Anonymize)
		if err != nil {
			return a, badAddon("%s", err.Error())
		}
		c.Anonymize = &rules
	}
	if req.AnonymizeSQL != nil {
		if !postgresOnly {
			return a, badAddon("anonymization statements apply to Postgres add-ons only")
		}
		sql := strings.TrimSpace(*req.AnonymizeSQL)
		if err := addons.CheckSQL(sql, store.MaxAnonymizeSQLBytes); err != nil {
			return a, badAddon("%s", err.Error())
		}
		c.AnonymizeSQL = &sql
	}
	if req.BackupKeep != nil {
		if !postgresOnly {
			return a, badAddon("backups apply to Postgres add-ons only")
		}
		if *req.BackupKeep < 0 || *req.BackupKeep > store.MaxBackupKeep {
			return a, badAddon("backup_keep must be between 0 (no daily backups) and %d", store.MaxBackupKeep)
		}
		c.BackupKeep = req.BackupKeep
	}
	a, err := st.UpdateAddon(ctx, a, c)
	if errors.Is(err, store.ErrNotFound) {
		return a, &AddonError{http.StatusConflict, "the add-on is being deleted"}
	}
	if errors.Is(err, store.ErrInvalid) {
		return a, badAddon("invalid add-on settings")
	}
	return a, err
}

// DeleteAddon starts deleting a; confirm must repeat its name, because its
// data and backups go with it.
func DeleteAddon(ctx context.Context, st *store.Store, a store.Addon, confirm string) (store.Addon, error) {
	if confirm != a.Name {
		return a, badAddon("deleting an add-on deletes its data and backups: confirm must be the add-on's name (%s)", a.Name)
	}
	return st.DeleteAddon(ctx, a)
}

// AddonCredentials is what reveal returns.
type AddonCredentials struct {
	Addon string `json:"addon"`
	Kind  string `json:"kind"`
	// Branch is set when a branch database was asked for.
	Branch string `json:"branch,omitempty"`
	store.AddonConnection
	URL string `json:"url"`
	// Variables are the plain variable names with their values.
	Variables map[string]string `json:"variables"`
}

// RevealAddon returns the production credentials of a, or those of a
// branch database. The caller logs who asked.
func RevealAddon(ctx context.Context, st *store.Store, a store.Addon, branch string) (AddonCredentials, error) {
	var c store.AddonConnection
	var err error
	if branch == "" {
		c, err = st.ProductionConnection(ctx, a)
	} else {
		c, err = st.BranchConnection(ctx, a, branch)
		if errors.Is(err, store.ErrNotFound) {
			return AddonCredentials{}, &AddonError{http.StatusNotFound, fmt.Sprintf("the add-on has no database for branch %q", branch)}
		}
	}
	if err != nil {
		return AddonCredentials{}, err
	}
	return AddonCredentials{Addon: a.Name, Kind: a.Kind, Branch: branch, AddonConnection: c, URL: c.URL(), Variables: c.Vars()}, nil
}

// RotateAddon requests a new password. redeploy redeploys what the aliases
// point at once it is applied.
func RotateAddon(ctx context.Context, st *store.Store, a store.Addon, redeploy bool) (store.Addon, error) {
	if a.Status != store.AddonReady {
		return a, &AddonError{http.StatusConflict, "the add-on is not ready"}
	}
	a, err := st.RequestAddonRotation(ctx, a, redeploy)
	if errors.Is(err, store.ErrBusy) {
		return a, &AddonError{http.StatusConflict, "a password rotation is already running"}
	}
	return a, err
}

// ResetBranch queues a fresh copy of production into a branch database.
func ResetBranch(ctx context.Context, st *store.Store, a store.Addon, branch string) (store.AddonBranch, error) {
	b, err := st.ResetAddonBranch(ctx, a, branch)
	switch {
	case errors.Is(err, store.ErrInvalid):
		return b, &AddonError{http.StatusConflict, "previews share the production database (preview_mode shared): there is no copy to refresh"}
	case errors.Is(err, store.ErrNotFound):
		return b, &AddonError{http.StatusNotFound, fmt.Sprintf("the add-on has no database for branch %q", branch)}
	case errors.Is(err, store.ErrBusy):
		return b, &AddonError{http.StatusConflict, "a copy of this branch is already running"}
	}
	return b, err
}

// CreateBackup queues a backup now.
func CreateBackup(ctx context.Context, st *store.Store, a store.Addon) (store.AddonBackup, error) {
	switch {
	case a.Kind != store.AddonPostgres:
		return store.AddonBackup{}, badAddon("backups apply to Postgres add-ons only")
	case a.Status != store.AddonReady:
		return store.AddonBackup{}, &AddonError{http.StatusConflict, "the add-on is not ready"}
	}
	b, err := st.CreateManualBackup(ctx, a)
	if errors.Is(err, store.ErrBusy) {
		return b, &AddonError{http.StatusConflict, "a backup is already running"}
	}
	return b, err
}

// RestoreBackup replaces the production database with backup id once the
// reconcile loop runs it; confirm must repeat the add-on's name.
func RestoreBackup(ctx context.Context, st *store.Store, a store.Addon, id int64, confirm, by string) (store.AddonBackup, error) {
	if confirm != a.Name {
		return store.AddonBackup{}, badAddon("restoring replaces the production database: confirm must be the add-on's name (%s)", a.Name)
	}
	if a.Status != store.AddonReady {
		return store.AddonBackup{}, &AddonError{http.StatusConflict, "the add-on is not ready"}
	}
	b, err := st.RequestRestore(ctx, a, id, by)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return b, &AddonError{http.StatusNotFound, "backup not found"}
	case errors.Is(err, store.ErrInvalid):
		return b, &AddonError{http.StatusConflict, "only a succeeded backup that was not pruned can be restored"}
	case errors.Is(err, store.ErrBusy):
		return b, &AddonError{http.StatusConflict, "a restore is already running"}
	}
	return b, err
}

// ---- handlers ----

// addonFail answers an error of the helpers above.
func (s *Server) addonFail(w http.ResponseWriter, err error) {
	var ae *AddonError
	if errors.As(err, &ae) {
		writeError(w, ae.Status, ae.Msg)
		return
	}
	s.internalError(w, err)
}

// lookupAddon loads the {addon} add-on of app.
func (s *Server) lookupAddon(w http.ResponseWriter, r *http.Request, app store.App) (store.Addon, bool) {
	a, err := s.Store.GetAddon(r.Context(), app.ID, r.PathValue("addon"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "add-on not found")
		return a, false
	}
	if err != nil {
		s.internalError(w, err)
		return a, false
	}
	return a, true
}

func (s *Server) appAddon(w http.ResponseWriter, r *http.Request) (store.App, store.Addon, bool) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return app, store.Addon{}, false
	}
	a, ok := s.lookupAddon(w, r, app)
	return app, a, ok
}

func (s *Server) listAddons(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	views, err := AddonViews(r.Context(), s.Store, app)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) createAddon(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var req CreateAddonRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a, err := CreateAddon(r.Context(), s.Store, app, req)
	if err != nil {
		s.addonFail(w, err)
		return
	}
	s.Log.Info("addon created", "app", app.Name, "addon", a.Name, "kind", a.Kind, "plan", a.Plan, "by", principal(r).Login)
	s.writeAddon(w, r, app, a, http.StatusCreated)
}

func (s *Server) writeAddon(w http.ResponseWriter, r *http.Request, app store.App, a store.Addon, status int) {
	v, err := AddonViewOf(r.Context(), s.Store, app, a)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, status, v)
}

func (s *Server) getAddon(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.appAddon(w, r)
	if !ok {
		return
	}
	s.writeAddon(w, r, app, a, http.StatusOK)
}

func (s *Server) updateAddon(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.appAddon(w, r)
	if !ok {
		return
	}
	var req UpdateAddonRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a, err := UpdateAddon(r.Context(), s.Store, a, req)
	if err != nil {
		s.addonFail(w, err)
		return
	}
	s.Log.Info("addon updated", "app", app.Name, "addon", a.Name, "by", principal(r).Login)
	s.writeAddon(w, r, app, a, http.StatusOK)
}

func (s *Server) deleteAddon(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.appAddon(w, r)
	if !ok {
		return
	}
	// ?confirm= or {"confirm": ...}: some clients drop DELETE bodies.
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	confirm := r.URL.Query().Get("confirm")
	if confirm == "" {
		confirm = body.Confirm
	}
	a, err := DeleteAddon(r.Context(), s.Store, a, confirm)
	if err != nil {
		s.addonFail(w, err)
		return
	}
	s.Log.Info("addon deletion started", "app", app.Name, "addon", a.Name, "by", principal(r).Login)
	writeJSON(w, http.StatusAccepted, addonView(a, false))
}

func (s *Server) revealAddon(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.appAddon(w, r)
	if !ok {
		return
	}
	var body struct {
		Branch string `json:"branch"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	creds, err := RevealAddon(r.Context(), s.Store, a, body.Branch)
	if err != nil {
		s.addonFail(w, err)
		return
	}
	// Audit trail: who saw which credentials.
	s.Log.Info("addon credentials revealed", "app", app.Name, "addon", a.Name, "branch", body.Branch,
		"by", principal(r).Login)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, creds)
}

func (s *Server) rotateAddon(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.appAddon(w, r)
	if !ok {
		return
	}
	body := struct {
		Redeploy *bool `json:"redeploy"`
	}{}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	redeploy := body.Redeploy == nil || *body.Redeploy
	a, err := RotateAddon(r.Context(), s.Store, a, redeploy)
	if err != nil {
		s.addonFail(w, err)
		return
	}
	s.Log.Info("addon rotation requested", "app", app.Name, "addon", a.Name, "redeploy", redeploy, "by", principal(r).Login)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"addon": a.Name, "rotating": true, "redeploy": redeploy,
		"note": rotationNote(redeploy),
	})
}

func rotationNote(redeploy bool) string {
	if redeploy {
		return "the new password is applied within a minute; then production and every preview alias are redeployed with it"
	}
	return "the new password is applied within a minute; running deployments keep their connections but need a redeploy for new ones"
}

func (s *Server) listAddonBranches(w http.ResponseWriter, r *http.Request) {
	_, a, ok := s.appAddon(w, r)
	if !ok {
		return
	}
	rows, err := s.Store.ListAddonBranches(r.Context(), a.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) resetAddonBranch(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.appAddon(w, r)
	if !ok {
		return
	}
	branch := r.PathValue("branch") // the mux unescapes it: feature%2Fx → feature/x
	if branch == "" {
		writeError(w, http.StatusBadRequest, "invalid branch")
		return
	}
	b, err := ResetBranch(r.Context(), s.Store, a, branch)
	if err != nil {
		s.addonFail(w, err)
		return
	}
	s.Log.Info("addon branch reset", "app", app.Name, "addon", a.Name, "branch", branch, "by", principal(r).Login)
	writeJSON(w, http.StatusAccepted, b)
}

func (s *Server) listAddonBackups(w http.ResponseWriter, r *http.Request) {
	_, a, ok := s.appAddon(w, r)
	if !ok {
		return
	}
	rows, err := s.Store.ListAddonBackups(r.Context(), a.ID, 100)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) createAddonBackup(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.appAddon(w, r)
	if !ok {
		return
	}
	b, err := CreateBackup(r.Context(), s.Store, a)
	if err != nil {
		s.addonFail(w, err)
		return
	}
	s.Log.Info("addon backup requested", "app", app.Name, "addon", a.Name, "backup", b.ID, "by", principal(r).Login)
	writeJSON(w, http.StatusAccepted, b)
}

func (s *Server) restoreAddonBackup(w http.ResponseWriter, r *http.Request) {
	app, a, ok := s.appAddon(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	b, err := RestoreBackup(r.Context(), s.Store, a, id, body.Confirm, principal(r).Login)
	if err != nil {
		s.addonFail(w, err)
		return
	}
	s.Log.Warn("addon restore requested", "app", app.Name, "addon", a.Name, "backup", id, "by", principal(r).Login)
	writeJSON(w, http.StatusAccepted, b)
}
