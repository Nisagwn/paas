package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/nisagwn/paas/internal/naming"
)

// Faz 22: managed databases (migration 015).
//
// An add-on is a Postgres or Redis server in the app's namespace (one
// StatefulSet, Service and PVC each, internal/deploy/addons.go). Its rows
// here are the source of truth: the reconcile loop (internal/addons)
// applies them to the cluster, moves add-ons from provisioning to ready and
// runs the Jobs that copy, drop, back up and restore databases.
//
// Deployments get connection variables through DeploymentEnv: production
// connects to the production database, a preview to its branch's copy
// (preview_mode copy / empty) or to production (shared). Variables a user
// set with the same name win.
//
// Credentials are sealed with the env keyring (envcrypt.go): with
// PAAS_ENV_KEY they are AES-256-GCM ciphertext bound to the app and add-on
// name (or, for a branch role, the add-on and database), without it they
// are stored as plaintext like env values. They never leave the store
// except through AddonSecretsOf and RevealAddon, which the API only offers
// to members.

// Add-on kinds.
const (
	AddonPostgres = "postgres"
	AddonRedis    = "redis"
)

// Add-on statuses.
const (
	AddonProvisioning = "provisioning"
	AddonReady        = "ready"
	AddonFailed       = "failed"
	AddonDeleting     = "deleting"
)

// Preview modes of a Postgres add-on.
const (
	PreviewCopy   = "copy"
	PreviewEmpty  = "empty"
	PreviewShared = "shared"
)

// Branch copy statuses.
const (
	BranchPending  = "pending"
	BranchCopying  = "copying"
	BranchReady    = "ready"
	BranchFailed   = "failed"
	BranchDeleting = "deleting"
)

// Backup statuses and triggers.
const (
	BackupPending   = "pending"
	BackupRunning   = "running"
	BackupSucceeded = "succeeded"
	BackupFailed    = "failed"
	BackupExpired   = "expired"

	BackupScheduled = "scheduled"
	BackupManual    = "manual"
)

// Connection constants. Postgres: the superuser "postgres" is used only by
// the platform's Jobs; deployments connect as "app" to the database "app"
// (production) or as the branch role to the branch database.
const (
	PostgresPort        = 5432
	RedisPort           = 6379
	PostgresAdminUser   = "postgres"
	PostgresAppUser     = "app"
	PostgresAppDatabase = "app"
)

// Limits.
const (
	MaxAddonsPerApp      = 5
	DefaultBackupKeep    = 7
	MaxBackupKeep        = 30
	MaxAnonymizeSQLBytes = 16384
)

var (
	// ErrAddonLimit: the app already has MaxAddonsPerApp add-ons.
	ErrAddonLimit = fmt.Errorf("an app can have at most %d add-ons", MaxAddonsPerApp)
	// ErrBusy: another operation of the same kind is still running (a
	// rotation, a copy, a backup or a restore).
	ErrBusy = errors.New("another operation is still running")
)

// AddonPlan is one of the fixed sizes an add-on can have. Quantities use
// Kubernetes syntax; memory is both request and limit.
type AddonPlan struct {
	ID         string `json:"id"`
	CPURequest string `json:"cpu_request"`
	CPULimit   string `json:"cpu_limit"`
	Memory     string `json:"memory"`
	// Storage of the data volume; Postgres gets a backup volume of the
	// same size.
	Storage string `json:"storage"`
	// MemoryMB sizes shared_buffers (Postgres) and maxmemory (Redis).
	MemoryMB int `json:"memory_mb"`
}

// AddonPlans, smallest first. The migration's CHECK mirrors the ids.
var AddonPlans = []AddonPlan{
	{ID: "hobby", CPURequest: "50m", CPULimit: "500m", Memory: "256Mi", Storage: "1Gi", MemoryMB: 256},
	{ID: "standard", CPURequest: "100m", CPULimit: "1", Memory: "512Mi", Storage: "5Gi", MemoryMB: 512},
	{ID: "pro", CPURequest: "250m", CPULimit: "2", Memory: "1Gi", Storage: "20Gi", MemoryMB: 1024},
}

// DefaultAddonPlan is the plan of an add-on created without one.
const DefaultAddonPlan = "hobby"

// GetAddonPlan returns the plan with id.
func GetAddonPlan(id string) (AddonPlan, bool) {
	for _, p := range AddonPlans {
		if p.ID == id {
			return p, true
		}
	}
	return AddonPlan{}, false
}

// ValidAddonKind reports whether kind is an add-on kind.
func ValidAddonKind(kind string) bool { return kind == AddonPostgres || kind == AddonRedis }

// ValidPreviewMode reports whether mode is a preview mode.
func ValidPreviewMode(mode string) bool {
	return mode == PreviewCopy || mode == PreviewEmpty || mode == PreviewShared
}

// DefaultAddonName is the name an add-on of kind gets when none is given;
// only add-ons with another name get prefixed variables.
func DefaultAddonName(kind string) string {
	if kind == AddonRedis {
		return "cache"
	}
	return "db"
}

// Addon is one add-on row, without its credentials.
type Addon struct {
	ID          int64  `json:"id"`
	AppID       int64  `json:"app_id"`
	AppName     string `json:"app"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Plan        string `json:"plan"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
	PreviewMode string `json:"preview_mode"`
	// Anonymize are the column rules as written ("users.email: email").
	Anonymize    []string `json:"anonymize"`
	AnonymizeSQL string   `json:"anonymize_sql,omitempty"`
	BackupKeep   int      `json:"backup_keep"`
	// Rotating: a new password was requested and is not applied yet.
	Rotating       bool       `json:"rotating"`
	RotateRedeploy bool       `json:"-"`
	SecretsVersion int        `json:"secrets_version"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ReadyAt        *time.Time `json:"ready_at,omitempty"`
}

// Object is the Kubernetes name of the add-on's objects.
func (a Addon) Object() string { return naming.AddonObject(a.Name) }

// Host is the add-on's Service in the app's namespace. ".svc" without the
// cluster domain resolves through the pods' search path on any cluster.
func (a Addon) Host() string { return a.Object() + "." + naming.Namespace(a.AppName) + ".svc" }

// Port the add-on listens on.
func (a Addon) Port() int {
	if a.Kind == AddonRedis {
		return RedisPort
	}
	return PostgresPort
}

// HasBranches reports whether previews get databases of their own.
func (a Addon) HasBranches() bool {
	return a.Kind == AddonPostgres && (a.PreviewMode == PreviewCopy || a.PreviewMode == PreviewEmpty)
}

// EnvPrefix is "" for an add-on with its kind's default name, otherwise
// the name in upper case with "_" ("analytics-db" → "ANALYTICS_DB_").
func (a Addon) EnvPrefix() string {
	if a.Name == DefaultAddonName(a.Kind) {
		return ""
	}
	return strings.ToUpper(strings.ReplaceAll(a.Name, "-", "_")) + "_"
}

// AddonSecrets are the add-on's passwords.
type AddonSecrets struct {
	// Admin is the Postgres superuser's password ("" for Redis).
	Admin string `json:"admin,omitempty"`
	// Password of the application role (Postgres) or requirepass (Redis).
	Password string `json:"password"`
}

// AddonConnection is where a deployment connects to.
type AddonConnection struct {
	Kind     string `json:"kind"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	Database string `json:"database,omitempty"`
}

// URL is DATABASE_URL (Postgres) or REDIS_URL (Redis). Traffic stays inside
// the cluster network, guarded by the namespace's NetworkPolicy, hence no TLS.
func (c AddonConnection) URL() string {
	hostPort := c.Host + ":" + strconv.Itoa(c.Port)
	if c.Kind == AddonRedis {
		return (&url.URL{Scheme: "redis", User: url.UserPassword("default", c.Password), Host: hostPort}).String()
	}
	return (&url.URL{Scheme: "postgres", User: url.UserPassword(c.User, c.Password), Host: hostPort,
		Path: "/" + c.Database, RawQuery: "sslmode=disable"}).String()
}

// Vars are the variables of the connection, without prefix.
func (c AddonConnection) Vars() map[string]string {
	if c.Kind == AddonRedis {
		return map[string]string{"REDIS_URL": c.URL()}
	}
	return map[string]string{
		"DATABASE_URL": c.URL(),
		"PGHOST":       c.Host,
		"PGPORT":       strconv.Itoa(c.Port),
		"PGUSER":       c.User,
		"PGPASSWORD":   c.Password,
		"PGDATABASE":   c.Database,
	}
}

// AddonVarNames lists the variables a's connection provides, sorted, with
// its prefix when it has one; plain tells whether the unprefixed names are
// also set (the add-on is its kind's primary one).
func AddonVarNames(a Addon, plain bool) []string {
	base := []string{"DATABASE_URL", "PGDATABASE", "PGHOST", "PGPASSWORD", "PGPORT", "PGUSER"}
	if a.Kind == AddonRedis {
		base = []string{"REDIS_URL"}
	}
	var out []string
	if plain {
		out = append(out, base...)
	}
	if p := a.EnvPrefix(); p != "" {
		for _, k := range base {
			out = append(out, p+k)
		}
	}
	return out
}

// AddonBranch is one preview branch's database, without its password.
type AddonBranch struct {
	ID         int64      `json:"id"`
	AddonID    int64      `json:"addon_id"`
	Branch     string     `json:"branch"`
	Database   string     `json:"database"`
	Mode       string     `json:"mode"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	Warning    string     `json:"warning,omitempty"`
	SnapshotAt *time.Time `json:"snapshot_at,omitempty"`
	SizeBytes  *int64     `json:"size_bytes,omitempty"`
	Generation int        `json:"generation"`
	Attempts   int        `json:"attempts"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// AddonBackup is one logical backup.
type AddonBackup struct {
	ID                int64      `json:"id"`
	AddonID           int64      `json:"addon_id"`
	Job               string     `json:"job"`
	Trigger           string     `json:"trigger"`
	Status            string     `json:"status"`
	Error             string     `json:"error,omitempty"`
	SizeBytes         *int64     `json:"size_bytes,omitempty"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	RestoreStatus     string     `json:"restore_status,omitempty"`
	RestoreError      string     `json:"restore_error,omitempty"`
	RestoreBy         string     `json:"restore_by,omitempty"`
	RestoreStartedAt  *time.Time `json:"restore_started_at,omitempty"`
	RestoreFinishedAt *time.Time `json:"restore_finished_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

// File is the dump's name in the backup volume.
func (b AddonBackup) File() string { return b.Job + ".dump" }

// ---- sealing ----

func addonAAD(appID int64, addon, field string) []byte {
	return []byte("paas/addon\x00" + strconv.FormatInt(appID, 10) + "\x00" + addon + "\x00" + field)
}

func branchAAD(addonID int64, database string) []byte {
	return []byte("paas/addon_branch\x00" + strconv.FormatInt(addonID, 10) + "\x00" + database)
}

// seal encrypts plain under the current key; without a keyring it is kept
// as is with a NULL key id (like env values).
func (s *Store) seal(plain string, aad []byte) (string, sql.NullString, error) {
	if s.env == nil {
		return plain, sql.NullString{}, nil
	}
	ct, err := s.env.Encrypt([]byte(plain), aad)
	if err != nil {
		return "", sql.NullString{}, err
	}
	return ct, sql.NullString{String: s.env.CurrentID(), Valid: true}, nil
}

func (s *Store) unseal(stored string, keyID sql.NullString, aad []byte) (string, error) {
	if !keyID.Valid {
		return stored, nil
	}
	if s.env == nil {
		return "", ErrEnvKeyMissing
	}
	plain, err := s.env.Decrypt(stored, aad)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (s *Store) sealSecrets(appID int64, addon, field string, sec AddonSecrets, keyed bool) (string, error) {
	raw, err := json.Marshal(sec)
	if err != nil {
		return "", err
	}
	if !keyed {
		return string(raw), nil
	}
	ct, err := s.env.Encrypt(raw, addonAAD(appID, addon, field))
	return ct, err
}

func (s *Store) openSecrets(appID int64, addon, field, stored string, keyID sql.NullString) (AddonSecrets, error) {
	plain, err := s.unseal(stored, keyID, addonAAD(appID, addon, field))
	if err != nil {
		return AddonSecrets{}, fmt.Errorf("add-on %s credentials: %w", addon, err)
	}
	var sec AddonSecrets
	if err := json.Unmarshal([]byte(plain), &sec); err != nil {
		return AddonSecrets{}, fmt.Errorf("add-on %s credentials: %w", addon, err)
	}
	return sec, nil
}

// NewPassword returns 160 random bits as 40 hex characters: safe in URLs,
// shell words and command line arguments without quoting.
func NewPassword() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// ---- add-ons ----

const addonCols = `ad.id, ad.app_id, a.name, ad.kind, ad.name, ad.plan, ad.status, ad.message, ad.preview_mode,
	ad.anonymize, ad.anonymize_sql, ad.backup_keep, ad.next_secrets IS NOT NULL, ad.rotate_redeploy,
	ad.secrets_version, ad.created_at, ad.updated_at, ad.ready_at`

const addonFrom = ` FROM addons ad JOIN apps a ON a.id = ad.app_id `

func scanAddon(row interface{ Scan(...any) error }) (Addon, error) {
	var a Addon
	var anon []byte
	err := row.Scan(&a.ID, &a.AppID, &a.AppName, &a.Kind, &a.Name, &a.Plan, &a.Status, &a.Message, &a.PreviewMode,
		&anon, &a.AnonymizeSQL, &a.BackupKeep, &a.Rotating, &a.RotateRedeploy,
		&a.SecretsVersion, &a.CreatedAt, &a.UpdatedAt, &a.ReadyAt)
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(anon, &a.Anonymize); err != nil {
		return a, fmt.Errorf("add-on %d anonymize rules: %w", a.ID, err)
	}
	if a.Anonymize == nil {
		a.Anonymize = []string{}
	}
	return a, nil
}

func (s *Store) queryAddons(ctx context.Context, where string, args ...any) ([]Addon, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+addonCols+addonFrom+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Addon{}
	for rows.Next() {
		a, err := scanAddon(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) getAddon(ctx context.Context, where string, args ...any) (Addon, error) {
	a, err := scanAddon(s.db.QueryRowContext(ctx, `SELECT `+addonCols+addonFrom+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// NewAddon is what CreateAddon needs; the caller validated it.
type NewAddon struct {
	Kind, Name, Plan, PreviewMode string
}

// CreateAddon stores a new add-on in status provisioning with fresh
// passwords. Errors: ErrConflict (name taken), ErrAddonLimit, ErrInvalid
// (a value the schema refuses).
func (s *Store) CreateAddon(ctx context.Context, appID int64, n NewAddon) (Addon, error) {
	sec := AddonSecrets{Password: NewPassword()}
	if n.Kind == AddonPostgres {
		sec.Admin = NewPassword()
	}
	keyed := s.env != nil
	stored, err := s.sealSecrets(appID, n.Name, "secrets", sec, keyed)
	if err != nil {
		return Addon{}, err
	}
	var keyID sql.NullString
	if keyed {
		keyID = sql.NullString{String: s.env.CurrentID(), Valid: true}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Addon{}, err
	}
	defer tx.Rollback()
	// The app row lock serializes concurrent creates for the limit check.
	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM addons WHERE app_id = a.id) FROM apps a WHERE a.id = $1 FOR UPDATE`, appID).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return Addon{}, ErrNotFound
	}
	if err != nil {
		return Addon{}, err
	}
	if count >= MaxAddonsPerApp {
		return Addon{}, ErrAddonLimit
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO addons (app_id, kind, name, plan, preview_mode, secrets, key_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		appID, n.Kind, n.Name, n.Plan, n.PreviewMode, stored, keyID).Scan(&id)
	if err = addonWriteError(err); err != nil {
		return Addon{}, err
	}
	if err := tx.Commit(); err != nil {
		return Addon{}, err
	}
	return s.GetAddonByID(ctx, id)
}

// addonWriteError maps constraint violations to store errors.
func addonWriteError(err error) error {
	var pqErr *pq.Error
	switch {
	case err == nil:
		return nil
	case isUniqueViolation(err):
		return ErrConflict
	case errors.As(err, &pqErr) && pqErr.Code == "23514": // check_violation
		return ErrInvalid
	}
	return err
}

// ListAddons returns the app's add-ons, oldest first.
func (s *Store) ListAddons(ctx context.Context, appID int64) ([]Addon, error) {
	return s.queryAddons(ctx, `WHERE ad.app_id = $1 ORDER BY ad.id`, appID)
}

// AppAddons returns the add-ons of the app named app, oldest first (the
// deployer sizes the namespace quota with them).
func (s *Store) AppAddons(ctx context.Context, app string) ([]Addon, error) {
	return s.queryAddons(ctx, `WHERE a.name = $1 ORDER BY ad.id`, app)
}

// GetAddon returns the app's add-on called name.
func (s *Store) GetAddon(ctx context.Context, appID int64, name string) (Addon, error) {
	return s.getAddon(ctx, `WHERE ad.app_id = $1 AND ad.name = $2`, appID, name)
}

// GetAddonByID returns an add-on by id.
func (s *Store) GetAddonByID(ctx context.Context, id int64) (Addon, error) {
	return s.getAddon(ctx, `WHERE ad.id = $1`, id)
}

// AddonChange changes some settings of an add-on; nil fields stay.
type AddonChange struct {
	Plan         *string
	PreviewMode  *string
	Anonymize    *[]string
	AnonymizeSQL *string
	BackupKeep   *int
}

// UpdateAddon applies c. A deleting add-on answers ErrNotFound.
func (s *Store) UpdateAddon(ctx context.Context, a Addon, c AddonChange) (Addon, error) {
	var anon []byte
	if c.Anonymize != nil {
		rules := *c.Anonymize
		if rules == nil {
			rules = []string{}
		}
		var err error
		if anon, err = json.Marshal(rules); err != nil {
			return a, err
		}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE addons SET
			plan = COALESCE($2, plan),
			preview_mode = COALESCE($3, preview_mode),
			anonymize = COALESCE($4::jsonb, anonymize),
			anonymize_sql = COALESCE($5, anonymize_sql),
			backup_keep = COALESCE($6, backup_keep),
			updated_at = now(),
			reconciled_at = '-infinity'
		WHERE id = $1 AND status <> 'deleting'`,
		a.ID, c.Plan, c.PreviewMode, nullBytes(anon), c.AnonymizeSQL, c.BackupKeep)
	if err = addonWriteError(err); err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return a, ErrNotFound
	}
	return s.GetAddonByID(ctx, a.ID)
}

func nullBytes(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

// DeleteAddon starts the deletion of an add-on: the reconcile loop deletes
// its objects (volumes included) and then the row. Deployments keep their
// variables until they are redeployed.
func (s *Store) DeleteAddon(ctx context.Context, a Addon) (Addon, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE addons SET status = 'deleting', updated_at = now(), reconciled_at = '-infinity'
		WHERE id = $1`, a.ID)
	if err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return a, ErrNotFound
	}
	return s.GetAddonByID(ctx, a.ID)
}

// RemoveAddon deletes the row of a deleted add-on (its branches and
// backups go with it). Only deleting add-ons are removed.
func (s *Store) RemoveAddon(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM addons WHERE id = $1 AND status = 'deleting'`, id)
	return err
}

// SetAddonState records what the reconcile loop saw. A deleting add-on
// keeps its status; ready_at is set the first time it is ready.
func (s *Store) SetAddonState(ctx context.Context, id int64, status, message string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE addons SET
			status = CASE WHEN status = 'deleting' THEN status ELSE $2 END,
			message = $3,
			ready_at = CASE WHEN $2 = 'ready' THEN COALESCE(ready_at, now()) ELSE ready_at END,
			updated_at = CASE WHEN status <> $2 OR message <> $3 THEN now() ELSE updated_at END
		WHERE id = $1`, id, status, message)
	return err
}

// ClaimDueAddons returns the add-ons not reconciled for every and marks
// them reconciled now. SKIP LOCKED lets several control plane replicas
// share the work (like ClaimDueRollouts).
func (s *Store) ClaimDueAddons(ctx context.Context, every time.Duration) ([]Addon, error) {
	return s.claimAddons(ctx, `reconciled_at <= now() - make_interval(secs => $1)`, every.Seconds())
}

// ClaimAppAddons claims the app's add-ons that were not reconciled during
// the last minGap (a kick: a deployment waits for them).
func (s *Store) ClaimAppAddons(ctx context.Context, appID int64, minGap time.Duration) ([]Addon, error) {
	return s.claimAddons(ctx, `app_id = $2 AND reconciled_at <= now() - make_interval(secs => $1)`,
		minGap.Seconds(), appID)
}

func (s *Store) claimAddons(ctx context.Context, where string, args ...any) ([]Addon, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE addons SET reconciled_at = now()
		WHERE id IN (SELECT id FROM addons WHERE `+where+` ORDER BY id FOR UPDATE SKIP LOCKED)
		RETURNING id`, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return s.queryAddons(ctx, `WHERE ad.id = ANY($1) ORDER BY ad.id`, pq.Array(ids))
}

// AddonSecretsOf returns the add-on's current passwords and, while a
// rotation is pending, the next ones.
func (s *Store) AddonSecretsOf(ctx context.Context, a Addon) (cur AddonSecrets, next *AddonSecrets, err error) {
	var stored string
	var nextStored sql.NullString
	var keyID sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT secrets, next_secrets, key_id FROM addons WHERE id = $1`, a.ID).
		Scan(&stored, &nextStored, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return cur, nil, ErrNotFound
	}
	if err != nil {
		return cur, nil, err
	}
	if cur, err = s.openSecrets(a.AppID, a.Name, "secrets", stored, keyID); err != nil {
		return cur, nil, err
	}
	if nextStored.Valid {
		n, err := s.openSecrets(a.AppID, a.Name, "next", nextStored.String, keyID)
		if err != nil {
			return cur, nil, err
		}
		next = &n
	}
	return cur, next, nil
}

// ProductionConnection is where production deployments connect to.
func (s *Store) ProductionConnection(ctx context.Context, a Addon) (AddonConnection, error) {
	sec, _, err := s.AddonSecretsOf(ctx, a)
	if err != nil {
		return AddonConnection{}, err
	}
	return productionConnection(a, sec), nil
}

func productionConnection(a Addon, sec AddonSecrets) AddonConnection {
	c := AddonConnection{Kind: a.Kind, Host: a.Host(), Port: a.Port(), Password: sec.Password}
	if a.Kind == AddonPostgres {
		c.User, c.Database = PostgresAppUser, PostgresAppDatabase
	}
	return c
}

// RequestAddonRotation stores a new application password; the reconcile
// loop applies it (Postgres: ALTER ROLE in a Job; Redis: the pod restarts
// with it) and then makes it current. redeploy also redeploys what the
// aliases point at, so they connect with the new password. ErrBusy while a
// rotation is pending.
func (s *Store) RequestAddonRotation(ctx context.Context, a Addon, redeploy bool) (Addon, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	var stored string
	var next, keyID sql.NullString
	var status string
	err = tx.QueryRowContext(ctx, `SELECT secrets, next_secrets, key_id, status FROM addons WHERE id = $1 FOR UPDATE`, a.ID).
		Scan(&stored, &next, &keyID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if status == AddonDeleting {
		return a, ErrNotFound
	}
	if next.Valid {
		return a, ErrBusy
	}
	cur, err := s.openSecrets(a.AppID, a.Name, "secrets", stored, keyID)
	if err != nil {
		return a, err
	}
	n := cur
	n.Password = NewPassword()
	// Both are sealed again under the current key, so one key id covers them.
	keyed := s.env != nil
	curStored, err := s.sealSecrets(a.AppID, a.Name, "secrets", cur, keyed)
	if err != nil {
		return a, err
	}
	nextStored, err := s.sealSecrets(a.AppID, a.Name, "next", n, keyed)
	if err != nil {
		return a, err
	}
	var newKey sql.NullString
	if keyed {
		newKey = sql.NullString{String: s.env.CurrentID(), Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE addons SET secrets = $2, next_secrets = $3, key_id = $4, rotate_redeploy = $5,
			updated_at = now(), reconciled_at = '-infinity'
		WHERE id = $1`, a.ID, curStored, nextStored, newKey, redeploy); err != nil {
		return a, err
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	return s.GetAddonByID(ctx, a.ID)
}

// CompleteAddonRotation makes the pending password current and reports
// whether the rotation asked for redeploys.
func (s *Store) CompleteAddonRotation(ctx context.Context, a Addon) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var next, keyID sql.NullString
	var redeploy bool
	err = tx.QueryRowContext(ctx, `SELECT next_secrets, key_id, rotate_redeploy FROM addons WHERE id = $1 FOR UPDATE`, a.ID).
		Scan(&next, &keyID, &redeploy)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !next.Valid) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	sec, err := s.openSecrets(a.AppID, a.Name, "next", next.String, keyID)
	if err != nil {
		return false, err
	}
	keyed := s.env != nil
	stored, err := s.sealSecrets(a.AppID, a.Name, "secrets", sec, keyed)
	if err != nil {
		return false, err
	}
	var newKey sql.NullString
	if keyed {
		newKey = sql.NullString{String: s.env.CurrentID(), Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE addons SET secrets = $2, key_id = $3, next_secrets = NULL, rotate_redeploy = false,
			secrets_version = secrets_version + 1, message = '', updated_at = now()
		WHERE id = $1`, a.ID, stored, newKey); err != nil {
		return false, err
	}
	return redeploy, tx.Commit()
}

// CancelAddonRotation drops a pending rotation that could not be applied;
// the current password stays.
func (s *Store) CancelAddonRotation(ctx context.Context, a Addon, message string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE addons SET next_secrets = NULL, rotate_redeploy = false, message = $2, updated_at = now()
		WHERE id = $1`, a.ID, message)
	return err
}

// AliasTargets returns the deployments the app's aliases point at
// (production and every branch preview), e.g. to redeploy them after a
// password rotation.
func (s *Store) AliasTargets(ctx context.Context, appID int64) ([]Deployment, error) {
	return s.queryDeployments(ctx, `
		SELECT `+deploymentCols+` FROM deployments d JOIN apps a ON a.id = d.app_id
		WHERE d.id IN (SELECT deployment_id FROM aliases WHERE app_id = $1) AND d.status = 'ready'
		ORDER BY d.id`, appID)
}

// ---- deployment variables ----

// AddonEnv returns the variables the app's ready add-ons give deployment d
// (DeploymentEnv merges them under the user's variables). Of each kind, the
// add-on with the default name (else the oldest) sets the plain names
// (DATABASE_URL, PGHOST, ..., REDIS_URL); every add-on with another name
// also sets them with its prefix (ANALYTICS_DATABASE_URL). A preview whose
// branch database is not ready gets no variables of that add-on.
func (s *Store) AddonEnv(ctx context.Context, d Deployment) (map[string]string, error) {
	addons, err := s.ListAddons(ctx, d.AppID)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	primary := PrimaryAddons(addons)
	for _, a := range addons {
		if a.Status != AddonReady {
			continue
		}
		c, ok, err := s.deploymentConnection(ctx, a, d)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		for k, v := range c.Vars() {
			if primary[a.Kind] == a.ID {
				env[k] = v
			}
			if p := a.EnvPrefix(); p != "" {
				env[p+k] = v
			}
		}
	}
	return env, nil
}

// PrimaryAddons picks, per kind, the ready add-on that sets the plain
// variable names: the one with the default name, else the oldest.
func PrimaryAddons(addons []Addon) map[string]int64 {
	out := map[string]int64{}
	for _, a := range addons {
		if a.Status == AddonReady && a.Name == DefaultAddonName(a.Kind) {
			out[a.Kind] = a.ID
		}
	}
	for _, a := range addons { // oldest first
		if _, ok := out[a.Kind]; !ok && a.Status == AddonReady {
			out[a.Kind] = a.ID
		}
	}
	return out
}

// deploymentConnection is where d connects to a; ok is false when its
// branch database is not ready.
func (s *Store) deploymentConnection(ctx context.Context, a Addon, d Deployment) (AddonConnection, bool, error) {
	sec, _, err := s.AddonSecretsOf(ctx, a)
	if err != nil {
		return AddonConnection{}, false, err
	}
	if d.Target == EnvProduction || !a.HasBranches() {
		return productionConnection(a, sec), true, nil
	}
	b, pw, err := s.addonBranchWithPassword(ctx, a, d.Branch)
	if errors.Is(err, ErrNotFound) {
		return AddonConnection{}, false, nil
	}
	if err != nil {
		return AddonConnection{}, false, err
	}
	if b.Status != BranchReady {
		return AddonConnection{}, false, nil
	}
	return AddonConnection{Kind: a.Kind, Host: a.Host(), Port: a.Port(),
		User: b.Database, Password: pw, Database: b.Database}, true, nil
}

// BranchConnection is where the preview deployments of branch connect to
// (the CLI's psql hint); ErrNotFound when the branch has no database.
func (s *Store) BranchConnection(ctx context.Context, a Addon, branch string) (AddonConnection, error) {
	b, pw, err := s.addonBranchWithPassword(ctx, a, branch)
	if err != nil {
		return AddonConnection{}, err
	}
	return AddonConnection{Kind: a.Kind, Host: a.Host(), Port: a.Port(),
		User: b.Database, Password: pw, Database: b.Database}, nil
}

// ---- branch databases ----

const branchCols = `id, addon_id, branch, database, mode, status, error, warning, snapshot_at, size_bytes,
	generation, attempts, created_at, updated_at`

func scanBranch(row interface{ Scan(...any) error }) (AddonBranch, error) {
	var b AddonBranch
	err := row.Scan(&b.ID, &b.AddonID, &b.Branch, &b.Database, &b.Mode, &b.Status, &b.Error, &b.Warning,
		&b.SnapshotAt, &b.SizeBytes, &b.Generation, &b.Attempts, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

// branchMode is the mode a new copy of a gets.
func branchMode(a Addon) string {
	if a.PreviewMode == PreviewEmpty {
		return PreviewEmpty
	}
	return PreviewCopy
}

// EnsureAddonBranch returns the branch's database row, creating it
// (pending, with a fresh role password) when there is none. A failed one
// is queued again with the add-on's current mode: a new push retries. A
// row being deleted is returned as is; the caller waits until it is gone
// and calls again.
func (s *Store) EnsureAddonBranch(ctx context.Context, a Addon, branch string) (AddonBranch, error) {
	db := naming.BranchDatabase(branch)
	pw, keyID, err := s.seal(NewPassword(), branchAAD(a.ID, db))
	if err != nil {
		return AddonBranch{}, err
	}
	b, err := scanBranch(s.db.QueryRowContext(ctx, `
		INSERT INTO addon_branches (addon_id, branch, database, mode, password, key_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (addon_id, branch) DO UPDATE SET status = 'pending', mode = EXCLUDED.mode,
			error = '', warning = '', generation = addon_branches.generation + 1, attempts = 0, updated_at = now()
		WHERE addon_branches.status = 'failed'
		RETURNING `+branchCols, a.ID, branch, db, branchMode(a), pw, keyID))
	if errors.Is(err, sql.ErrNoRows) {
		return s.GetAddonBranch(ctx, a.ID, branch)
	}
	if isUniqueViolation(err) {
		// Another branch maps to the same database name (a hash collision
		// of naming.BranchDatabase, practically never).
		return AddonBranch{}, fmt.Errorf("branch %q: database %s is taken by another branch: %w", branch, db, ErrConflict)
	}
	return b, err
}

// GetAddonBranch returns the add-on's database row of branch.
func (s *Store) GetAddonBranch(ctx context.Context, addonID int64, branch string) (AddonBranch, error) {
	b, err := scanBranch(s.db.QueryRowContext(ctx, `SELECT `+branchCols+` FROM addon_branches
		WHERE addon_id = $1 AND branch = $2`, addonID, branch))
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

// ListAddonBranches returns the add-on's branch databases by branch.
func (s *Store) ListAddonBranches(ctx context.Context, addonID int64) ([]AddonBranch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+branchCols+` FROM addon_branches
		WHERE addon_id = $1 ORDER BY branch`, addonID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AddonBranch{}
	for rows.Next() {
		b, err := scanBranch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) addonBranchWithPassword(ctx context.Context, a Addon, branch string) (AddonBranch, string, error) {
	var stored string
	var keyID sql.NullString
	var b AddonBranch
	err := s.db.QueryRowContext(ctx, `SELECT `+branchCols+`, password, key_id FROM addon_branches
		WHERE addon_id = $1 AND branch = $2`, a.ID, branch).
		Scan(&b.ID, &b.AddonID, &b.Branch, &b.Database, &b.Mode, &b.Status, &b.Error, &b.Warning,
			&b.SnapshotAt, &b.SizeBytes, &b.Generation, &b.Attempts, &b.CreatedAt, &b.UpdatedAt, &stored, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return b, "", ErrNotFound
	}
	if err != nil {
		return b, "", err
	}
	pw, err := s.unseal(stored, keyID, branchAAD(a.ID, b.Database))
	if err != nil {
		return b, "", fmt.Errorf("branch database %s: %w", b.Database, err)
	}
	return b, pw, nil
}

// AddonBranchPasswords returns the role password of every branch database
// of the add-on by database name (the reconcile loop keeps them in a
// Secret the copy Jobs read).
func (s *Store) AddonBranchPasswords(ctx context.Context, a Addon) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT database, password, key_id FROM addon_branches WHERE addon_id = $1`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var db, stored string
		var keyID sql.NullString
		if err := rows.Scan(&db, &stored, &keyID); err != nil {
			return nil, err
		}
		pw, err := s.unseal(stored, keyID, branchAAD(a.ID, db))
		if err != nil {
			return nil, fmt.Errorf("branch database %s: %w", db, err)
		}
		out[db] = pw
	}
	return out, rows.Err()
}

// ResetAddonBranch queues a fresh copy of production into the branch's
// database ("Kopyayı yenile"). Errors: ErrNotFound (no such branch, or it
// is being deleted), ErrBusy (a copy is pending or running), ErrInvalid
// (the add-on shares production with previews).
func (s *Store) ResetAddonBranch(ctx context.Context, a Addon, branch string) (AddonBranch, error) {
	if !a.HasBranches() {
		return AddonBranch{}, ErrInvalid
	}
	b, err := scanBranch(s.db.QueryRowContext(ctx, `
		UPDATE addon_branches SET status = 'pending', mode = $3, error = '', warning = '',
			generation = generation + 1, attempts = 0, updated_at = now()
		WHERE addon_id = $1 AND branch = $2 AND status IN ('ready', 'failed')
		RETURNING `+branchCols, a.ID, branch, branchMode(a)))
	if errors.Is(err, sql.ErrNoRows) {
		cur, gerr := s.GetAddonBranch(ctx, a.ID, branch)
		switch {
		case gerr != nil:
			return cur, gerr
		case cur.Status == BranchDeleting:
			return cur, ErrNotFound
		}
		return cur, ErrBusy
	}
	if err == nil {
		s.db.ExecContext(ctx, `UPDATE addons SET reconciled_at = '-infinity' WHERE id = $1`, a.ID)
	}
	return b, err
}

// StartAddonBranch moves a pending copy of generation gen to copying.
func (s *Store) StartAddonBranch(ctx context.Context, id int64, gen int) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE addon_branches SET status = 'copying', attempts = attempts + 1, updated_at = now()
		WHERE id = $1 AND generation = $2 AND status = 'pending'`, id, gen)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// BranchResult is what a finished copy Job reported.
type BranchResult struct {
	// Mode is what was done: copy, or empty after a fallback.
	Mode       string
	Warning    string
	SnapshotAt *time.Time
	SizeBytes  *int64
}

// FinishAddonBranch records a successful copy of generation gen; a result
// of an older generation (the copy was reset meanwhile) is dropped.
func (s *Store) FinishAddonBranch(ctx context.Context, id int64, gen int, r BranchResult) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE addon_branches SET status = 'ready', mode = COALESCE(NULLIF($3, ''), mode), warning = $4,
			snapshot_at = $5, size_bytes = $6, error = '', updated_at = now()
		WHERE id = $1 AND generation = $2 AND status = 'copying'`, id, gen, r.Mode, r.Warning, r.SnapshotAt, r.SizeBytes)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FailAddonBranch records a failed copy of generation gen.
func (s *Store) FailAddonBranch(ctx context.Context, id int64, gen int, reason string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE addon_branches SET status = 'failed', error = $3, updated_at = now()
		WHERE id = $1 AND generation = $2 AND status IN ('pending', 'copying')`, id, gen, reason)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// NoteAddonBranchError records a problem of a branch database without
// changing its status (a drop that failed and is retried).
func (s *Store) NoteAddonBranchError(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE addon_branches SET error = $2 WHERE id = $1`, id, reason)
	return err
}

// RequeueAddonBranch puts a copy whose Job disappeared back to pending.
func (s *Store) RequeueAddonBranch(ctx context.Context, id int64, gen int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE addon_branches SET status = 'pending', updated_at = now()
		WHERE id = $1 AND generation = $2 AND status = 'copying'`, id, gen)
	return err
}

// markBranchDatabasesDeletingTx marks the branch's databases of every
// add-on of the app for deletion (DeleteBranch).
func markBranchDatabasesDeletingTx(ctx context.Context, tx *sql.Tx, appID int64, branch string) (int, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE addon_branches b SET status = 'deleting', updated_at = now()
		FROM addons ad WHERE ad.id = b.addon_id AND ad.app_id = $1 AND b.branch = $2 AND b.status <> 'deleting'`,
		appID, branch)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE addons SET reconciled_at = '-infinity' WHERE app_id = $1`, appID); err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ExpireAddonBranches marks for deletion the add-on's branch databases
// whose branch has no live preview deployment any more (queued, building,
// deploying or ready and not retired): the branch was deleted, its pull
// request closed or its previews retired by the TTL. grace keeps a row
// that changed recently (a deployment is about to be queued). It returns
// the branches.
func (s *Store) ExpireAddonBranches(ctx context.Context, a Addon, grace time.Duration) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE addon_branches b SET status = 'deleting', updated_at = now()
		WHERE b.addon_id = $1 AND b.status IN ('ready', 'failed')
		  AND b.updated_at < now() - make_interval(secs => $3)
		  AND NOT EXISTS (
		    SELECT 1 FROM deployments d
		    WHERE d.app_id = $2 AND d.branch = b.branch AND d.target = 'preview' AND d.retired_at IS NULL
		      AND d.status IN ('queued', 'building', 'deploying', 'ready'))
		RETURNING b.branch`, a.ID, a.AppID, grace.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteAddonBranches marks the branch databases of an add-on for deletion
// when the add-on stops giving previews their own databases (preview mode
// shared).
func (s *Store) DeleteAddonBranches(ctx context.Context, a Addon) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE addon_branches SET status = 'deleting', updated_at = now()
		WHERE addon_id = $1 AND status <> 'deleting'`, a.ID)
	return err
}

// RemoveAddonBranch deletes the row of a dropped branch database.
func (s *Store) RemoveAddonBranch(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM addon_branches WHERE id = $1 AND status = 'deleting'`, id)
	return err
}

// ---- backups ----

const backupCols = `id, addon_id, job, trigger, status, error, size_bytes, started_at, finished_at,
	restore_status, restore_error, restore_by, restore_started_at, restore_finished_at, created_at`

func scanBackup(row interface{ Scan(...any) error }) (AddonBackup, error) {
	var b AddonBackup
	err := row.Scan(&b.ID, &b.AddonID, &b.Job, &b.Trigger, &b.Status, &b.Error, &b.SizeBytes, &b.StartedAt,
		&b.FinishedAt, &b.RestoreStatus, &b.RestoreError, &b.RestoreBy, &b.RestoreStartedAt, &b.RestoreFinishedAt,
		&b.CreatedAt)
	return b, err
}

// BackupJobName is the Job of a manual backup with row id.
func BackupJobName(a Addon, id int64) string {
	return a.Object() + "-backup-m" + strconv.FormatInt(id, 10)
}

// CreateManualBackup queues a backup now (the reconcile loop starts its
// Job). ErrBusy while another manual backup is pending or running;
// ErrInvalid for a Redis add-on.
func (s *Store) CreateManualBackup(ctx context.Context, a Addon) (AddonBackup, error) {
	if a.Kind != AddonPostgres {
		return AddonBackup{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AddonBackup{}, err
	}
	defer tx.Rollback()
	var busy bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM addon_backups WHERE addon_id = $1 AND trigger = 'manual'
			AND status IN ('pending', 'running'))
		FROM addons WHERE id = $1 FOR UPDATE`, a.ID).Scan(&busy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AddonBackup{}, ErrNotFound
		}
		return AddonBackup{}, err
	}
	if busy {
		return AddonBackup{}, ErrBusy
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT nextval('addon_backups_id_seq')`).Scan(&id); err != nil {
		return AddonBackup{}, err
	}
	b, err := scanBackup(tx.QueryRowContext(ctx, `
		INSERT INTO addon_backups (id, addon_id, job, trigger) VALUES ($1, $2, $3, 'manual')
		RETURNING `+backupCols, id, a.ID, BackupJobName(a, id)))
	if err != nil {
		return b, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE addons SET reconciled_at = '-infinity' WHERE id = $1`, a.ID); err != nil {
		return b, err
	}
	return b, tx.Commit()
}

// ListAddonBackups returns the add-on's backups, newest first.
func (s *Store) ListAddonBackups(ctx context.Context, addonID int64, limit int) ([]AddonBackup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupCols+` FROM addon_backups
		WHERE addon_id = $1 ORDER BY COALESCE(started_at, created_at) DESC, id DESC LIMIT $2`, addonID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AddonBackup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetAddonBackup returns one backup of the add-on.
func (s *Store) GetAddonBackup(ctx context.Context, addonID, id int64) (AddonBackup, error) {
	b, err := scanBackup(s.db.QueryRowContext(ctx, `SELECT `+backupCols+` FROM addon_backups
		WHERE addon_id = $1 AND id = $2`, addonID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

// BackupRun is the state of a backup Job as the cluster reports it.
type BackupRun struct {
	Job        string
	Trigger    string
	Status     string // running, succeeded or failed
	Error      string
	SizeBytes  *int64
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// RecordBackupRun creates or updates the backup row of a Job. A finished
// row (succeeded, failed, expired) is never moved back.
func (s *Store) RecordBackupRun(ctx context.Context, addonID int64, r BackupRun) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO addon_backups (addon_id, job, trigger, status, error, size_bytes, started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (addon_id, job) DO UPDATE SET status = EXCLUDED.status, error = EXCLUDED.error,
			size_bytes = COALESCE(EXCLUDED.size_bytes, addon_backups.size_bytes),
			started_at = COALESCE(addon_backups.started_at, EXCLUDED.started_at),
			finished_at = COALESCE(EXCLUDED.finished_at, addon_backups.finished_at)
		WHERE addon_backups.status IN ('pending', 'running')`,
		addonID, r.Job, r.Trigger, r.Status, r.Error, r.SizeBytes, r.StartedAt, r.FinishedAt)
	return err
}

// ExpireAddonBackups marks the add-on's succeeded backups whose files the
// backup Job pruned (not in kept) as expired; only rows that finished
// before the pruning backup (before) are considered.
func (s *Store) ExpireAddonBackups(ctx context.Context, addonID int64, kept []string, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE addon_backups SET status = 'expired'
		WHERE addon_id = $1 AND status = 'succeeded' AND finished_at <= $3 AND NOT (job || '.dump' = ANY($2))`,
		addonID, pq.Array(kept), before)
	return err
}

// RequestRestore asks the reconcile loop to restore backup id into the
// production database. Errors: ErrNotFound, ErrInvalid (the backup did not
// succeed or expired), ErrBusy (another restore is pending or running).
func (s *Store) RequestRestore(ctx context.Context, a Addon, id int64, by string) (AddonBackup, error) {
	b, err := s.GetAddonBackup(ctx, a.ID, id)
	if err != nil {
		return b, err
	}
	if b.Status != BackupSucceeded {
		return b, ErrInvalid
	}
	b, err = scanBackup(s.db.QueryRowContext(ctx, `
		UPDATE addon_backups SET restore_status = 'pending', restore_error = '', restore_by = $3,
			restore_started_at = NULL, restore_finished_at = NULL
		WHERE addon_id = $1 AND id = $2 AND status = 'succeeded'
		RETURNING `+backupCols, a.ID, id, by))
	if isUniqueViolation(err) {
		return b, ErrBusy
	}
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrInvalid
	}
	if err == nil {
		s.db.ExecContext(ctx, `UPDATE addons SET reconciled_at = '-infinity' WHERE id = $1`, a.ID)
	}
	return b, err
}

// SetBackupStatus moves a manual backup row (pending → running → ...).
func (s *Store) SetBackupStatus(ctx context.Context, id int64, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE addon_backups SET status = $2, error = $3,
			started_at = CASE WHEN $2 = 'running' THEN COALESCE(started_at, now()) ELSE started_at END,
			finished_at = CASE WHEN $2 IN ('succeeded', 'failed') THEN COALESCE(finished_at, now()) ELSE finished_at END
		WHERE id = $1 AND status IN ('pending', 'running')`, id, status, errMsg)
	return err
}

// SetRestoreStatus records the progress of a restore.
func (s *Store) SetRestoreStatus(ctx context.Context, id int64, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE addon_backups SET restore_status = $2, restore_error = $3,
			restore_started_at = CASE WHEN $2 = 'running' THEN COALESCE(restore_started_at, now()) ELSE restore_started_at END,
			restore_finished_at = CASE WHEN $2 IN ('succeeded', 'failed') THEN now() ELSE restore_finished_at END
		WHERE id = $1`, id, status, errMsg)
	return err
}

// ---- key rotation (envcrypt.go) ----

// addonKeyStats counts sealed add-on rows per key id ("" = plaintext).
const addonKeyStatsQuery = `
	SELECT COALESCE(key_id, ''), count(*) FROM addons GROUP BY 1
	UNION ALL
	SELECT COALESCE(key_id, ''), count(*) FROM addon_branches GROUP BY 1`

// reencryptAddons seals add-on and branch credentials under the current
// key inside tx; see ReencryptEnv.
func (s *Store) reencryptAddons(ctx context.Context, tx *sql.Tx) (int, error) {
	cur := s.env.CurrentID()
	type addonRow struct {
		id, appID    int64
		name, stored string
		next, keyID  sql.NullString
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, app_id, name, secrets, next_secrets, key_id FROM addons
		WHERE key_id IS DISTINCT FROM $1 ORDER BY id FOR UPDATE`, cur)
	if err != nil {
		return 0, err
	}
	var addons []addonRow
	for rows.Next() {
		var r addonRow
		if err := rows.Scan(&r.id, &r.appID, &r.name, &r.stored, &r.next, &r.keyID); err != nil {
			rows.Close()
			return 0, err
		}
		addons = append(addons, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range addons {
		sec, err := s.openSecrets(r.appID, r.name, "secrets", r.stored, r.keyID)
		if err != nil {
			return 0, err
		}
		stored, err := s.sealSecrets(r.appID, r.name, "secrets", sec, true)
		if err != nil {
			return 0, err
		}
		var next sql.NullString
		if r.next.Valid {
			n, err := s.openSecrets(r.appID, r.name, "next", r.next.String, r.keyID)
			if err != nil {
				return 0, err
			}
			ct, err := s.sealSecrets(r.appID, r.name, "next", n, true)
			if err != nil {
				return 0, err
			}
			next = sql.NullString{String: ct, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE addons SET secrets = $2, next_secrets = $3, key_id = $4 WHERE id = $1`,
			r.id, stored, next, cur); err != nil {
			return 0, err
		}
	}

	type branchRow struct {
		id, addonID int64
		db, stored  string
		keyID       sql.NullString
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT id, addon_id, database, password, key_id FROM addon_branches
		WHERE key_id IS DISTINCT FROM $1 ORDER BY id FOR UPDATE`, cur)
	if err != nil {
		return 0, err
	}
	var branches []branchRow
	for rows.Next() {
		var r branchRow
		if err := rows.Scan(&r.id, &r.addonID, &r.db, &r.stored, &r.keyID); err != nil {
			rows.Close()
			return 0, err
		}
		branches = append(branches, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range branches {
		pw, err := s.unseal(r.stored, r.keyID, branchAAD(r.addonID, r.db))
		if err != nil {
			return 0, fmt.Errorf("branch database %s: %w", r.db, err)
		}
		ct, keyID, err := s.seal(pw, branchAAD(r.addonID, r.db))
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE addon_branches SET password = $2, key_id = $3 WHERE id = $1`,
			r.id, ct, keyID); err != nil {
			return 0, err
		}
	}
	return len(addons) + len(branches), nil
}
