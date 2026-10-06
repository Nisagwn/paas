package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Faz 21: gradual rollouts (canary) and automatic rollback (migration 014).
//
// A production deployment that becomes ready normally takes the production
// alias at once (mode instant). With mode canary, MarkReady leaves the
// production alias on the current deployment and opens a rollout instead:
// the routing layer then splits the production hostnames (and the custom
// domains, which follow production) between the two deployments by weight.
// The controller (internal/rollout) compares their request metrics step by
// step and either moves the weight forward, finally the alias itself
// (promoted), or sets the weight to zero (rolled_back). With mode guarded,
// the alias moves at once and a guard rollout watches the new production;
// rolling it back moves the alias back to the previous deployment.
//
// Every state change is a compare-and-set on (state, step) inside a
// transaction, so the controller loop, user actions and new deployments
// cannot overwrite each other's decisions.

// Rollout modes (rollout_settings.mode; rollouts.mode is canary or guarded).
const (
	RolloutInstant = "instant"
	RolloutGuarded = "guarded"
	RolloutCanary  = "canary"
)

// Rollout states.
const (
	RolloutRunning    = "running"
	RolloutPaused     = "paused"
	RolloutPromoted   = "promoted"
	RolloutRolledBack = "rolled_back"
	RolloutAborted    = "aborted"
	RolloutSuperseded = "superseded"
)

// Manual rollout actions (API, web UI, CLI).
const (
	ActionPromote  = "promote"
	ActionAbort    = "abort"
	ActionPause    = "pause"
	ActionResume   = "resume"
	ActionRollback = "rollback"
)

// Limits of the settings, mirrored by the CHECK constraints.
const (
	MinRolloutStep = 2 * time.Minute // metrics are per minute
	MaxRolloutStep = 24 * time.Hour
	MaxCanarySteps = 10
)

// RolloutSettings is an app's rollout configuration.
type RolloutSettings struct {
	Mode string `json:"mode"`
	// Steps are the canary weights in percent, strictly increasing and
	// ending with 100; reaching 100 promotes the deployment.
	Steps       []int `json:"steps"`
	StepSeconds int   `json:"step_seconds"`
	// MaxErrorPct is the highest 5xx rate (percent) the new deployment may
	// have; MaxErrorIncreasePct the most it may exceed the current
	// production's rate (percentage points).
	MaxErrorPct         float64 `json:"max_error_pct"`
	MaxErrorIncreasePct float64 `json:"max_error_increase_pct"`
	// MaxP95Ms caps the p95 latency (0: off); MaxP95Factor caps it relative
	// to the current production's p95 (0: off).
	MaxP95Ms     int     `json:"max_p95_ms"`
	MaxP95Factor float64 `json:"max_p95_factor"`
	// MinRequests the new deployment must serve in a step before its rates
	// count.
	MinRequests  int        `json:"min_requests"`
	GuardSeconds int        `json:"guard_seconds"`
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
}

// DefaultRolloutSettings are the settings of an app without a row.
func DefaultRolloutSettings() RolloutSettings {
	return RolloutSettings{
		Mode: RolloutInstant, Steps: []int{10, 50, 100}, StepSeconds: 300,
		MaxErrorPct: 5, MaxErrorIncreasePct: 2, MaxP95Ms: 0, MaxP95Factor: 2,
		MinRequests: 50, GuardSeconds: 600,
	}
}

// StepDuration is how long one canary step lasts.
func (s RolloutSettings) StepDuration() time.Duration {
	return time.Duration(s.StepSeconds) * time.Second
}

// GuardDuration is how long a guarded switch is watched.
func (s RolloutSettings) GuardDuration() time.Duration {
	return time.Duration(s.GuardSeconds) * time.Second
}

// Validate checks the settings; the error message is meant for users.
func (s RolloutSettings) Validate() error {
	switch s.Mode {
	case RolloutInstant, RolloutGuarded, RolloutCanary:
	default:
		return errors.New(`mode must be "instant", "guarded" or "canary"`)
	}
	if len(s.Steps) < 2 || len(s.Steps) > MaxCanarySteps {
		return fmt.Errorf("steps must have 2 to %d weights, e.g. 10,50,100", MaxCanarySteps)
	}
	prev := 0
	for i, w := range s.Steps {
		last := i == len(s.Steps)-1
		if (last && w != 100) || (!last && (w < 1 || w > 99)) || w <= prev {
			return errors.New("steps must be strictly increasing percentages between 1 and 99, ending with 100")
		}
		prev = w
	}
	step := s.StepDuration()
	if step < MinRolloutStep || step > MaxRolloutStep {
		return fmt.Errorf("step_seconds must be between %d and %d (metrics are collected per minute)",
			int(MinRolloutStep.Seconds()), int(MaxRolloutStep.Seconds()))
	}
	guard := s.GuardDuration()
	if guard < MinRolloutStep || guard > MaxRolloutStep {
		return fmt.Errorf("guard_seconds must be between %d and %d",
			int(MinRolloutStep.Seconds()), int(MaxRolloutStep.Seconds()))
	}
	if !(s.MaxErrorPct > 0 && s.MaxErrorPct <= 100) {
		return errors.New("max_error_pct must be in (0, 100]")
	}
	if !(s.MaxErrorIncreasePct >= 0 && s.MaxErrorIncreasePct <= 100) {
		return errors.New("max_error_increase_pct must be in [0, 100]")
	}
	if s.MaxP95Ms < 0 || s.MaxP95Ms > 600000 {
		return errors.New("max_p95_ms must be between 0 (off) and 600000")
	}
	if !(s.MaxP95Factor == 0 || (s.MaxP95Factor >= 1 && s.MaxP95Factor <= 100)) {
		return errors.New("max_p95_factor must be 0 (off) or between 1 and 100")
	}
	if s.MinRequests < 1 || s.MinRequests > 1000000 {
		return errors.New("min_requests must be between 1 and 1000000")
	}
	return nil
}

// FormatSteps renders steps as "10,50,100".
func FormatSteps(steps []int) string {
	parts := make([]string, len(steps))
	for i, w := range steps {
		parts[i] = strconv.Itoa(w)
	}
	return strings.Join(parts, ",")
}

// ParseSteps reads "10,50,100" (spaces and a trailing % allowed).
func ParseSteps(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSuffix(strings.TrimSpace(p), "%")
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("steps: %q is not a number", p)
		}
		out = append(out, n)
	}
	return out, nil
}

const rolloutSettingsCols = `mode, steps, step_seconds, max_error_pct, max_error_increase_pct,
	max_p95_ms, max_p95_factor, min_requests, guard_seconds, updated_at`

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getRolloutSettings(ctx context.Context, q querier, appID int64) (RolloutSettings, error) {
	s := DefaultRolloutSettings()
	var steps []int64
	var updated time.Time
	err := q.QueryRowContext(ctx, `SELECT `+rolloutSettingsCols+` FROM rollout_settings WHERE app_id = $1`, appID).
		Scan(&s.Mode, pq.Array(&steps), &s.StepSeconds, &s.MaxErrorPct, &s.MaxErrorIncreasePct,
			&s.MaxP95Ms, &s.MaxP95Factor, &s.MinRequests, &s.GuardSeconds, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.Steps = make([]int, len(steps))
	for i, w := range steps {
		s.Steps[i] = int(w)
	}
	s.UpdatedAt = &updated
	return s, nil
}

// GetRolloutSettings returns the app's settings (defaults without a row).
func (s *Store) GetRolloutSettings(ctx context.Context, appID int64) (RolloutSettings, error) {
	return getRolloutSettings(ctx, s.db, appID)
}

// SetRolloutSettings validates and stores the app's settings. A running
// rollout keeps the settings it started with.
func (s *Store) SetRolloutSettings(ctx context.Context, appID int64, rs RolloutSettings) (RolloutSettings, error) {
	if err := rs.Validate(); err != nil {
		return rs, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	steps := make([]int64, len(rs.Steps))
	for i, w := range rs.Steps {
		steps[i] = int64(w)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rollout_settings (app_id, mode, steps, step_seconds, max_error_pct, max_error_increase_pct,
			max_p95_ms, max_p95_factor, min_requests, guard_seconds)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (app_id) DO UPDATE SET mode = EXCLUDED.mode, steps = EXCLUDED.steps,
			step_seconds = EXCLUDED.step_seconds, max_error_pct = EXCLUDED.max_error_pct,
			max_error_increase_pct = EXCLUDED.max_error_increase_pct, max_p95_ms = EXCLUDED.max_p95_ms,
			max_p95_factor = EXCLUDED.max_p95_factor, min_requests = EXCLUDED.min_requests,
			guard_seconds = EXCLUDED.guard_seconds, updated_at = now()`,
		appID, rs.Mode, pq.Array(steps), rs.StepSeconds, rs.MaxErrorPct, rs.MaxErrorIncreasePct,
		rs.MaxP95Ms, rs.MaxP95Factor, rs.MinRequests, rs.GuardSeconds)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23503" { // foreign key: app deleted
			return rs, ErrNotFound
		}
		return rs, err
	}
	return s.GetRolloutSettings(ctx, appID)
}

// ---- rollouts ----

// RolloutSample is one side's traffic in a verdict's window.
type RolloutSample struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"` // 5xx
	// P95Ms is nil without a duration histogram (or without requests).
	P95Ms *float64 `json:"p95_ms,omitempty"`
}

// ErrorPct is the 5xx rate in percent (0 without requests).
func (s RolloutSample) ErrorPct() float64 {
	if s.Requests <= 0 {
		return 0
	}
	return 100 * float64(s.Errors) / float64(s.Requests)
}

// RolloutVerdict is one entry of a rollout's decision log.
type RolloutVerdict struct {
	At       time.Time      `json:"at"`
	Step     int            `json:"step"`
	Weight   int            `json:"weight"`
	Action   string         `json:"action"` // start, advance, promote, rollback, abort, pause, resume, supersede
	Reason   string         `json:"reason"`
	Canary   *RolloutSample `json:"canary,omitempty"`
	Baseline *RolloutSample `json:"baseline,omitempty"`
}

// Rollout is one canary or guard run.
type Rollout struct {
	ID      int64  `json:"id"`
	AppID   int64  `json:"app_id"`
	AppName string `json:"app_name"`
	// FromDeploymentID: the production deployment when the rollout started
	// (nil if it has been deleted since).
	FromDeploymentID *int64 `json:"from_deployment_id,omitempty"`
	ToDeploymentID   int64  `json:"to_deployment_id"`
	Mode             string `json:"mode"`
	Step             int    `json:"step"`
	// Weight is the new deployment's share of the production traffic.
	Weight        int              `json:"weight"`
	State         string           `json:"state"`
	Reason        string           `json:"reason"`
	Settings      RolloutSettings  `json:"settings"`
	Verdicts      []RolloutVerdict `json:"verdicts"`
	StepStartedAt time.Time        `json:"step_started_at"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	FinishedAt    *time.Time       `json:"finished_at,omitempty"`
}

// Active reports whether the rollout is running or paused.
func (r Rollout) Active() bool { return r.State == RolloutRunning || r.State == RolloutPaused }

// LastStep reports whether the rollout is at its last canary weight
// before 100 %.
func (r Rollout) LastStep() bool { return r.Step >= len(r.Settings.Steps)-2 }

const rolloutCols = `r.id, r.app_id, a.name, r.from_deployment_id, r.to_deployment_id, r.mode, r.step, r.weight,
	r.state, r.reason, r.settings, r.verdicts, r.step_started_at, r.created_at, r.updated_at, r.finished_at`

func scanRollout(row interface{ Scan(...any) error }) (Rollout, error) {
	var r Rollout
	var settings, verdicts []byte
	err := row.Scan(&r.ID, &r.AppID, &r.AppName, &r.FromDeploymentID, &r.ToDeploymentID, &r.Mode, &r.Step, &r.Weight,
		&r.State, &r.Reason, &settings, &verdicts, &r.StepStartedAt, &r.CreatedAt, &r.UpdatedAt, &r.FinishedAt)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(settings, &r.Settings); err != nil {
		return r, fmt.Errorf("rollout %d settings: %w", r.ID, err)
	}
	if err := json.Unmarshal(verdicts, &r.Verdicts); err != nil {
		return r, fmt.Errorf("rollout %d verdicts: %w", r.ID, err)
	}
	if r.Verdicts == nil {
		r.Verdicts = []RolloutVerdict{}
	}
	return r, nil
}

func (s *Store) rollouts(ctx context.Context, where string, args ...any) ([]Rollout, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+rolloutCols+` FROM rollouts r JOIN apps a ON a.id = r.app_id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Rollout{}
	for rows.Next() {
		r, err := scanRollout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRollout loads one rollout.
func (s *Store) GetRollout(ctx context.Context, id int64) (Rollout, error) {
	r, err := scanRollout(s.db.QueryRowContext(ctx,
		`SELECT `+rolloutCols+` FROM rollouts r JOIN apps a ON a.id = r.app_id WHERE r.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// ActiveRollout returns the app's running or paused rollout (ErrNotFound
// without one).
func (s *Store) ActiveRollout(ctx context.Context, appID int64) (Rollout, error) {
	r, err := scanRollout(s.db.QueryRowContext(ctx, `SELECT `+rolloutCols+`
		FROM rollouts r JOIN apps a ON a.id = r.app_id
		WHERE r.app_id = $1 AND r.state IN ('running', 'paused')`, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// ListRollouts returns the app's rollouts, newest first.
func (s *Store) ListRollouts(ctx context.Context, appID int64, limit int) ([]Rollout, error) {
	return s.rollouts(ctx, `WHERE r.app_id = $1 ORDER BY r.id DESC LIMIT $2`, appID, limit)
}

// ClaimDueRollouts returns the running rollouts not evaluated within
// every and marks them evaluated now. SKIP LOCKED lets several control
// plane replicas run the controller: each rollout is evaluated by one of
// them per period.
func (s *Store) ClaimDueRollouts(ctx context.Context, every time.Duration) ([]Rollout, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE rollouts SET evaluated_at = now()
		WHERE id IN (
			SELECT id FROM rollouts
			WHERE state = 'running' AND evaluated_at <= now() - make_interval(secs => $1)
			ORDER BY id FOR UPDATE SKIP LOCKED)
		RETURNING id`, every.Seconds())
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
	out := make([]Rollout, 0, len(ids))
	for _, id := range ids {
		r, err := s.GetRollout(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// ---- starting a rollout (MarkReady) ----

func appendVerdict(verdicts []RolloutVerdict, v RolloutVerdict) ([]byte, error) {
	if v.At.IsZero() {
		v.At = time.Now().UTC()
	}
	return json.Marshal(append(verdicts, v))
}

// endActiveTx ends the app's active rollout (if any) with state and reason
// and returns it. The caller holds a transaction.
func endActiveTx(ctx context.Context, tx *sql.Tx, appID int64, state, action, reason string) (*Rollout, error) {
	r, err := scanRollout(tx.QueryRowContext(ctx, `SELECT `+rolloutCols+`
		FROM rollouts r JOIN apps a ON a.id = r.app_id
		WHERE r.app_id = $1 AND r.state IN ('running', 'paused') FOR UPDATE OF r`, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	weight := r.Weight
	if r.Mode == RolloutCanary {
		weight = 0 // the canary leaves the production hostnames
	}
	verdicts, err := appendVerdict(r.Verdicts, RolloutVerdict{Step: r.Step, Weight: weight, Action: action, Reason: reason})
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rollouts SET state = $2, reason = $3, weight = $4, verdicts = $5, updated_at = now(), finished_at = now()
		WHERE id = $1`, r.ID, state, reason, weight, verdicts); err != nil {
		return nil, err
	}
	r.State, r.Reason, r.Weight = state, reason, weight
	return &r, nil
}

// planRolloutTx decides, for deployment d becoming ready with aliases,
// which aliases move and whether a rollout starts. It runs inside
// MarkReady's transaction.
//
//   - no production alias in aliases (preview), or the app has no
//     production deployment yet: nothing changes;
//   - d is older than the current production or the active canary: the
//     production alias stays (aliases only move forward);
//   - a promotion (origin promote) is an explicit switch: it aborts an
//     active rollout and moves the alias at once (and is guarded in
//     guarded mode);
//   - canary: the production alias stays, a canary rollout starts from
//     the current production; an active rollout is superseded;
//   - guarded: the alias moves, a guard rollout starts; an active one is
//     superseded;
//   - instant: the alias moves; an active rollout is superseded.
func planRolloutTx(ctx context.Context, tx *sql.Tx, d Deployment, aliases []AliasSpec) ([]AliasSpec, *Rollout, error) {
	prodIdx := -1
	for i, a := range aliases {
		if a.Kind == AliasProduction {
			prodIdx = i
		}
	}
	if prodIdx < 0 {
		return aliases, nil, nil
	}
	var cur int64
	err := tx.QueryRowContext(ctx, `
		SELECT deployment_id FROM aliases WHERE app_id = $1 AND kind = 'production' FOR UPDATE`, d.AppID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return aliases, nil, nil // first production deployment
	}
	if err != nil {
		return nil, nil, err
	}
	withoutProd := append(append([]AliasSpec{}, aliases[:prodIdx]...), aliases[prodIdx+1:]...)
	if cur >= d.ID {
		return aliases, nil, nil // MarkReady's forward-only rule keeps the alias
	}
	// An older build finishing during a canary of a newer one must not
	// take production either.
	var activeTo int64
	err = tx.QueryRowContext(ctx, `
		SELECT to_deployment_id FROM rollouts
		WHERE app_id = $1 AND state IN ('running', 'paused') AND mode = 'canary'`, d.AppID).Scan(&activeTo)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	if activeTo > d.ID {
		return withoutProd, nil, nil
	}

	settings, err := getRolloutSettings(ctx, tx, d.AppID)
	if err != nil {
		return nil, nil, err
	}
	mode := settings.Mode
	endState, endAction := RolloutSuperseded, "supersede"
	endReason := fmt.Sprintf("daha yeni bir production deploy'u geldi (#%d)", d.ID)
	if d.Origin == OriginPromote {
		endState, endAction = RolloutAborted, "abort"
		endReason = fmt.Sprintf("#%d deploy'u promote edildi", d.ID)
		if mode == RolloutCanary {
			mode = RolloutInstant // a promotion is an explicit, immediate switch
		}
	}
	if _, err := endActiveTx(ctx, tx, d.AppID, endState, endAction, endReason); err != nil {
		return nil, nil, err
	}
	if mode == RolloutInstant {
		return aliases, nil, nil
	}

	weight, out := 100, aliases
	start := fmt.Sprintf("anında geçiş (#%d → #%d); %s izleniyor", cur, d.ID, settings.GuardDuration())
	if mode == RolloutCanary {
		weight, out = settings.Steps[0], withoutProd
		start = fmt.Sprintf("canary başladı: #%d deploy'u trafiğin %%%d'unu alıyor, production #%d", d.ID, weight, cur)
	}
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return nil, nil, err
	}
	verdicts, err := appendVerdict(nil, RolloutVerdict{Weight: weight, Action: "start", Reason: start})
	if err != nil {
		return nil, nil, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO rollouts (app_id, from_deployment_id, to_deployment_id, mode, weight, settings, verdicts)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		d.AppID, cur, d.ID, mode, weight, settingsJSON, verdicts).Scan(&id); err != nil {
		return nil, nil, err
	}
	r, err := scanRollout(tx.QueryRowContext(ctx,
		`SELECT `+rolloutCols+` FROM rollouts r JOIN apps a ON a.id = r.app_id WHERE r.id = $1`, id))
	if err != nil {
		return nil, nil, err
	}
	return out, &r, nil
}

// abortActiveForRollback ends an active rollout when a user moves the
// production alias by hand (Rollback, promote of a production deployment).
func abortActiveForRollback(ctx context.Context, tx *sql.Tx, appID, target int64) error {
	_, err := endActiveTx(ctx, tx, appID, RolloutAborted, "abort",
		fmt.Sprintf("production elle #%d deploy'una taşındı", target))
	return err
}

// ---- transitions ----

// RolloutChange is one state change of a rollout.
type RolloutChange struct {
	// State is the new state; "" keeps it.
	State string
	// Step and Weight are the new step and weight (nil keeps them).
	Step, Weight *int
	Reason       string
	Verdict      *RolloutVerdict
	// RestartStep starts a new step window now (advance, resume).
	RestartStep bool
	// MoveProductionTo points the production alias at this deployment in
	// the same transaction (canary promotion, guard rollback); 0 leaves it.
	MoveProductionTo int64
}

// ErrStale is returned when a rollout changed since it was read (another
// replica, a user action or a new deployment got there first).
var ErrStale = errors.New("rollout changed meanwhile")

// UpdateRollout applies c to rollout r if its state and step are still
// those of r (compare and set) and returns the new row.
func (s *Store) UpdateRollout(ctx context.Context, r Rollout, c RolloutChange) (Rollout, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	cur, err := scanRollout(tx.QueryRowContext(ctx,
		`SELECT `+rolloutCols+` FROM rollouts r JOIN apps a ON a.id = r.app_id WHERE r.id = $1 FOR UPDATE OF r`, r.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if cur.State != r.State || cur.Step != r.Step {
		return cur, ErrStale
	}
	if c.MoveProductionTo != 0 {
		if err := moveProductionTx(ctx, tx, cur.AppID, c.MoveProductionTo); err != nil {
			return cur, err
		}
	}
	next := cur
	if c.State != "" {
		next.State = c.State
	}
	if c.Step != nil {
		next.Step = *c.Step
	}
	if c.Weight != nil {
		next.Weight = *c.Weight
	}
	if c.Reason != "" {
		next.Reason = c.Reason
	}
	verdicts := next.Verdicts
	if c.Verdict != nil {
		v := *c.Verdict
		if v.At.IsZero() {
			v.At = time.Now().UTC()
		}
		verdicts = append(verdicts, v)
	}
	vb, err := json.Marshal(verdicts)
	if err != nil {
		return cur, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE rollouts SET state = $2, step = $3, weight = $4, reason = $5, verdicts = $6, updated_at = now(),
			step_started_at = CASE WHEN $7 THEN now() ELSE step_started_at END,
			finished_at = CASE WHEN $2 IN ('running', 'paused') THEN NULL ELSE now() END
		WHERE id = $1`, cur.ID, next.State, next.Step, next.Weight, next.Reason, vb, c.RestartStep); err != nil {
		return cur, err
	}
	if err := tx.Commit(); err != nil {
		return cur, err
	}
	return s.GetRollout(ctx, r.ID)
}

// moveProductionTx points the production alias at deployment id, like
// Rollback: id must be a ready deployment of the app.
func moveProductionTx(ctx context.Context, tx *sql.Tx, appID, id int64) error {
	var status string
	err := tx.QueryRowContext(ctx, `
		SELECT status FROM deployments WHERE id = $1 AND app_id = $2 FOR SHARE`, id, appID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == StatusRetired {
		return ErrRetired
	}
	if status != StatusReady {
		return ErrNotReady
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE aliases SET deployment_id = $2, updated_at = now() WHERE app_id = $1 AND kind = 'production'`, appID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ManualRolloutAction applies a user's action to the app's active
// rollout:
//
//	promote  canary: the production alias moves to the new deployment now;
//	         guard: the watch ends successfully
//	rollback canary: weight 0; guard: the alias moves back
//	abort    the rollout ends; the production alias stays where it is
//	pause    running → paused (weights stay; no automatic decision)
//	resume   paused → running with a new step window
//
// ErrNotFound: no active rollout; ErrInvalid: the action does not apply in
// the rollout's state.
func (s *Store) ManualRolloutAction(ctx context.Context, appID int64, action, by string) (Rollout, error) {
	r, err := s.ActiveRollout(ctx, appID)
	if err != nil {
		return r, err
	}
	verb, ok := manualActionText[action]
	if !ok {
		return r, ErrInvalid
	}
	reason := by + " tarafından elle " + verb
	v := &RolloutVerdict{Step: r.Step, Action: action, Reason: reason}
	c := RolloutChange{Reason: reason, Verdict: v}
	switch action {
	case ActionPause:
		if r.State != RolloutRunning {
			return r, ErrInvalid
		}
		c.State = RolloutPaused
	case ActionResume:
		if r.State != RolloutPaused {
			return r, ErrInvalid
		}
		c.State, c.RestartStep = RolloutRunning, true
	case ActionPromote:
		c.State, c.Weight = RolloutPromoted, ptrInt(100)
		if r.Mode == RolloutCanary {
			c.MoveProductionTo = r.ToDeploymentID
		}
	case ActionRollback:
		c.State = RolloutRolledBack
		if r.Mode == RolloutCanary {
			c.Weight = ptrInt(0)
		} else if r.FromDeploymentID != nil {
			c.MoveProductionTo, c.Weight = *r.FromDeploymentID, ptrInt(0)
		} else {
			return r, ErrInvalid // the previous deployment is gone
		}
	case ActionAbort:
		c.State = RolloutAborted
		if r.Mode == RolloutCanary {
			c.Weight = ptrInt(0)
		}
	default:
		return r, ErrInvalid
	}
	if c.Weight != nil {
		v.Weight = *c.Weight
	} else {
		v.Weight = r.Weight
	}
	return s.UpdateRollout(ctx, r, c)
}

// manualActionText completes "<login> tarafından elle ...".
var manualActionText = map[string]string{
	ActionPromote:  "tamamlandı",
	ActionAbort:    "durduruldu",
	ActionPause:    "duraklatıldı",
	ActionResume:   "sürdürüldü",
	ActionRollback: "geri alındı",
}

func ptrInt(v int) *int { return &v }

// ---- routing ----

// SplitBackend is one side of a traffic split.
type SplitBackend struct {
	DeploymentID int64
	CommitSHA    string
	Generation   int
}

// TrafficSplit is what the routing layer needs to split an app's
// production traffic during a canary.
type TrafficSplit struct {
	RolloutID int64
	// Stable is the current production deployment; Canary the new one.
	Stable, Canary SplitBackend
	// CanaryWeight is the canary's share in percent (0-100).
	CanaryWeight int
	// Routes are the hostnames that split: the production alias and the
	// routed custom domains (they follow production).
	Routes []AliasRoute
}

// TrafficSplit returns the app's active canary split, or nil when the
// production hostnames go to the production alias alone (no canary, a
// guard rollout, or the previous deployment is gone).
func (s *Store) TrafficSplit(ctx context.Context, appID int64) (*TrafficSplit, error) {
	var sp TrafficSplit
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.weight, f.id, f.commit_sha, f.generation, t.id, t.commit_sha, t.generation
		FROM rollouts r
		JOIN deployments f ON f.id = r.from_deployment_id
		JOIN deployments t ON t.id = r.to_deployment_id
		WHERE r.app_id = $1 AND r.state IN ('running', 'paused') AND r.mode = 'canary'
		  AND t.status = 'ready' AND f.status = 'ready'`, appID).
		Scan(&sp.RolloutID, &sp.CanaryWeight, &sp.Stable.DeploymentID, &sp.Stable.CommitSHA, &sp.Stable.Generation,
			&sp.Canary.DeploymentID, &sp.Canary.CommitSHA, &sp.Canary.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	routes, err := s.AliasRoutes(ctx, appID)
	if err != nil {
		return nil, err
	}
	for _, r := range routes {
		if r.Kind == AliasProduction || r.Kind == AliasCustom {
			sp.Routes = append(sp.Routes, r)
		}
	}
	return &sp, nil
}

// ---- metrics ----

// WindowTraffic sums one deployment's request metrics over the minutes in
// [from, to).
func (s *Store) WindowTraffic(ctx context.Context, deploymentID int64, from, to time.Time) (MetricPoint, error) {
	p := MetricPoint{Time: from}
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(sum(requests), 0), COALESCE(sum(status_2xx), 0), COALESCE(sum(status_3xx), 0),
			COALESCE(sum(status_4xx), 0), COALESCE(sum(status_5xx), 0),
			COALESCE(sum(duration_sum), 0), COALESCE(sum(duration_count), 0)
		FROM request_metrics WHERE deployment_id = $1 AND minute >= $2 AND minute < $3`,
		deploymentID, from, to).Scan(&p.Requests, &p.Classes[0], &p.Classes[1], &p.Classes[2], &p.Classes[3],
		&p.DurationSum, &p.DurationCount)
	if err != nil {
		return p, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.key, sum(b.value::float8)
		FROM request_metrics, jsonb_each_text(duration_buckets) b
		WHERE deployment_id = $1 AND minute >= $2 AND minute < $3
		GROUP BY b.key`, deploymentID, from, to)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var v float64
		if err := rows.Scan(&key, &v); err != nil {
			return p, err
		}
		le, err := strconv.ParseFloat(key, 64)
		if err != nil {
			continue
		}
		if p.Buckets == nil {
			p.Buckets = map[float64]float64{}
		}
		p.Buckets[le] += v
	}
	return p, rows.Err()
}
