package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Faz 23: team plans, suspension, usage counters and the fair build queue.
// The limits themselves live in internal/plans; the store only keeps the
// plan name, the admin's per-team overrides and the numbers to compare.

// Team plans. They mirror the CHECK constraint on teams.plan.
const (
	PlanHobby     = "hobby"
	PlanPro       = "pro"
	PlanUnlimited = "unlimited"
)

// ValidPlan reports whether p is one of the three plans.
func ValidPlan(p string) bool { return p == PlanHobby || p == PlanPro || p == PlanUnlimited }

// SuspendedReason is the error of deployments canceled by a suspension.
const SuspendedReason = "canceled: the team was suspended"

// TeamPlan is a team with its plan, overrides and suspension.
type TeamPlan struct {
	Team
	Plan string `json:"plan"`
	// Overrides are the admin's per-team limits ({"projects": 50}); keys
	// that are missing follow the plan.
	Overrides       json.RawMessage `json:"overrides"`
	SuspendedAt     *time.Time      `json:"suspended_at,omitempty"`
	SuspendedReason string          `json:"suspended_reason,omitempty"`
}

// Suspended reports whether an admin suspended the team.
func (t TeamPlan) Suspended() bool { return t.SuspendedAt != nil }

const teamPlanCols = teamCols + `, t.plan, t.plan_limits, t.suspended_at, t.suspended_reason`

func scanTeamPlan(row interface{ Scan(...any) error }, extra ...any) (TeamPlan, error) {
	var tp TeamPlan
	var overrides []byte
	err := row.Scan(append([]any{&tp.ID, &tp.Slug, &tp.Name, &tp.CreatedAt, &tp.Plan, &overrides,
		&tp.SuspendedAt, &tp.SuspendedReason}, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return tp, ErrNotFound
	}
	tp.Overrides = json.RawMessage(overrides)
	return tp, err
}

// GetTeamPlan returns the plan of team id.
func (s *Store) GetTeamPlan(ctx context.Context, teamID int64) (TeamPlan, error) {
	return scanTeamPlan(s.db.QueryRowContext(ctx, `SELECT `+teamPlanCols+` FROM teams t WHERE t.id = $1`, teamID))
}

// TeamPlanOfApp returns the plan of the team that owns app id.
func (s *Store) TeamPlanOfApp(ctx context.Context, appID int64) (TeamPlan, error) {
	return scanTeamPlan(s.db.QueryRowContext(ctx, `SELECT `+teamPlanCols+` FROM teams t
		JOIN apps a ON a.team_id = t.id WHERE a.id = $1`, appID))
}

// TeamPlanOfAppName returns the plan of the team that owns the named app.
func (s *Store) TeamPlanOfAppName(ctx context.Context, app string) (TeamPlan, error) {
	return scanTeamPlan(s.db.QueryRowContext(ctx, `SELECT `+teamPlanCols+` FROM teams t
		JOIN apps a ON a.team_id = t.id WHERE a.name = $1`, app))
}

// SetTeamPlan changes a team's plan and replaces its overrides (nil or
// empty: none). The overrides must be a JSON object; internal/plans
// validates the keys and values.
func (s *Store) SetTeamPlan(ctx context.Context, teamID int64, plan string, overrides json.RawMessage) error {
	if !ValidPlan(plan) {
		return ErrInvalid
	}
	if len(overrides) == 0 || string(overrides) == "null" {
		overrides = json.RawMessage(`{}`)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE teams SET plan = $2, plan_limits = $3 WHERE id = $1`,
		teamID, plan, []byte(overrides))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SuspendTeam suspends a team: queued deployments of its apps are canceled
// at once, running ones are asked to stop (the worker cancels them at its
// next heartbeat). It returns the number of deployments it stopped.
func (s *Store) SuspendTeam(ctx context.Context, teamID int64, reason string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE teams SET suspended_at = COALESCE(suspended_at, now()),
		suspended_reason = $2 WHERE id = $1`, teamID, reason)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	// The same rows CancelDeployment writes, for every in-flight deployment.
	res, err = tx.ExecContext(ctx, `
		UPDATE deployments d SET
			status = CASE WHEN d.status = 'queued' THEN 'canceled' ELSE d.status END,
			error = CASE WHEN d.status = 'queued' THEN $2 ELSE d.error END,
			finished_at = CASE WHEN d.status = 'queued' THEN now() ELSE d.finished_at END,
			cleaned_at = CASE WHEN d.status = 'queued' THEN now() ELSE d.cleaned_at END,
			cancel_requested_at = COALESCE(d.cancel_requested_at, now())
		FROM apps a
		WHERE a.id = d.app_id AND a.team_id = $1 AND d.status IN ('queued', 'building', 'deploying')`,
		teamID, SuspendedReason)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}

// UnsuspendTeam lifts a suspension.
func (s *Store) UnsuspendTeam(ctx context.Context, teamID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE teams SET suspended_at = NULL, suspended_reason = '' WHERE id = $1`, teamID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TeamAppNames lists the names of a team's apps.
func (s *Store) TeamAppNames(ctx context.Context, teamID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM apps WHERE team_id = $1 ORDER BY name`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---- users ----

// SuspendedUser is a user an admin suspended.
type SuspendedUser struct {
	User
	SuspendedAt     time.Time `json:"suspended_at"`
	SuspendedReason string    `json:"suspended_reason"`
}

// SuspendUser suspends a user: sessions and tokens stop working.
func (s *Store) SuspendUser(ctx context.Context, userID int64, reason string) error {
	return s.setUserSuspension(ctx, `UPDATE users SET suspended_at = COALESCE(suspended_at, now()),
		suspended_reason = $2 WHERE id = $1`, userID, reason)
}

// UnsuspendUser lifts a user's suspension.
func (s *Store) UnsuspendUser(ctx context.Context, userID int64) error {
	return s.setUserSuspension(ctx, `UPDATE users SET suspended_at = NULL, suspended_reason = '' WHERE id = $1`, userID)
}

func (s *Store) setUserSuspension(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UserSuspended reports whether the user is suspended; an unknown user is
// not (the caller finds out it is gone elsewhere).
func (s *Store) UserSuspended(ctx context.Context, userID int64) (bool, error) {
	var suspended bool
	err := s.db.QueryRowContext(ctx, `SELECT suspended_at IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&suspended)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return suspended, err
}

// SuspendedUsers lists suspended users, most recent first.
func (s *Store) SuspendedUsers(ctx context.Context) ([]SuspendedUser, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+`, suspended_at, suspended_reason FROM users
		WHERE suspended_at IS NOT NULL ORDER BY suspended_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SuspendedUser{}
	for rows.Next() {
		var u SuspendedUser
		if err := rows.Scan(&u.ID, &u.GitHubID, &u.Login, &u.Name, &u.AvatarURL, &u.CreatedAt,
			&u.SuspendedAt, &u.SuspendedReason); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- usage ----

// TeamUsage are the counters a team's limits are compared with.
type TeamUsage struct {
	Projects int `json:"projects"`
	Domains  int `json:"domains"`
	// Addons excludes add-ons being deleted.
	Addons int `json:"addons"`
	// DeploymentsToday: created since the start of the day (UTC).
	DeploymentsToday int `json:"deployments_today"`
	// BuildSeconds: build time of deployments created this month (UTC).
	BuildSeconds int64 `json:"build_seconds"`
	// ActiveBuilds: deployments building or deploying now.
	ActiveBuilds int `json:"active_builds"`
}

// usageCols computes TeamUsage for the team id in column team (an
// expression); $1 is the start of the month, $2 the start of the day.
func usageCols(team string) string {
	return `(SELECT count(*) FROM apps ua WHERE ua.team_id = ` + team + `),
		(SELECT count(*) FROM app_domains dm JOIN apps ua ON ua.id = dm.app_id WHERE ua.team_id = ` + team + `),
		(SELECT count(*) FROM addons ad JOIN apps ua ON ua.id = ad.app_id WHERE ua.team_id = ` + team + ` AND ad.status <> 'deleting'),
		(SELECT count(*) FROM deployments ud JOIN apps ua ON ua.id = ud.app_id WHERE ua.team_id = ` + team + ` AND ud.created_at >= $2),
		(SELECT COALESCE(sum(ud.build_seconds), 0) FROM deployments ud JOIN apps ua ON ua.id = ud.app_id
			WHERE ua.team_id = ` + team + ` AND ud.created_at >= $1),
		(SELECT count(*) FROM deployments ud JOIN apps ua ON ua.id = ud.app_id
			WHERE ua.team_id = ` + team + ` AND ud.status IN ('building', 'deploying'))`
}

func (u *TeamUsage) dest() []any {
	return []any{&u.Projects, &u.Domains, &u.Addons, &u.DeploymentsToday, &u.BuildSeconds, &u.ActiveBuilds}
}

// TeamUsage counts a team's usage; monthStart and dayStart bound the
// monthly build minutes and the daily deployments.
func (s *Store) TeamUsage(ctx context.Context, teamID int64, monthStart, dayStart time.Time) (TeamUsage, error) {
	var u TeamUsage
	err := s.db.QueryRowContext(ctx, `SELECT `+usageCols("$3"), monthStart, dayStart, teamID).Scan(u.dest()...)
	return u, err
}

// TeamWithUsage is a row of the admin's team list.
type TeamWithUsage struct {
	TeamPlan
	Usage   TeamUsage `json:"usage"`
	Members int       `json:"members"`
	// Owners are the logins of the team's owners.
	Owners []string `json:"owners"`
}

// TeamsWithUsage lists every team with its plan and usage (admin view).
func (s *Store) TeamsWithUsage(ctx context.Context, monthStart, dayStart time.Time) ([]TeamWithUsage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+teamPlanCols+`, `+usageCols("t.id")+`,
		(SELECT count(*) FROM team_members m WHERE m.team_id = t.id),
		COALESCE((SELECT string_agg(u.login, ',' ORDER BY lower(u.login)) FROM team_members m
			JOIN users u ON u.id = m.user_id WHERE m.team_id = t.id AND m.role = 'owner'), '')
		FROM teams t ORDER BY t.id`, monthStart, dayStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamWithUsage{}
	for rows.Next() {
		var row TeamWithUsage
		var owners string
		extra := append(row.Usage.dest(), &row.Members, &owners)
		tp, err := scanTeamPlan(rows, extra...)
		if err != nil {
			return nil, err
		}
		row.TeamPlan = tp
		row.Owners = []string{}
		if owners != "" {
			row.Owners = splitComma(owners)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

// AddBuildSeconds records build time of a deployment (every attempt adds).
func (s *Store) AddBuildSeconds(ctx context.Context, id int64, seconds int) error {
	if seconds < 0 {
		seconds = 0
	}
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET build_seconds = build_seconds + $2 WHERE id = $1`, id, seconds)
	return err
}

// BuildSeconds returns the recorded build time of a deployment.
func (s *Store) BuildSeconds(ctx context.Context, id int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT build_seconds FROM deployments WHERE id = $1`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return n, err
}

// ---- the fair queue ----

// ClaimCaps bounds the deployments one team may build at once, by plan.
// A plan that is missing or negative has no bound; a team's override
// ("concurrent_builds" in its plan_limits) wins over its plan's value.
// A nil ByPlan applies no bounds at all (no plans configured).
type ClaimCaps struct {
	ByPlan map[string]int
}

func (c ClaimCaps) plan(p string) int {
	if v, ok := c.ByPlan[p]; ok {
		return v
	}
	return -1
}

// claimLockID serializes claims, so two workers cannot both see a team
// below its bound and claim one deployment each beyond it.
const claimLockID = 727275

// ClaimNextFor is ClaimNext with per-team bounds (Faz 23): it claims the
// oldest queued deployment of a team that is not suspended and is below
// its bound of concurrent builds. A team at its bound does not block the
// others: its deployments wait while the queue moves on.
func (s *Store) ClaimNextFor(ctx context.Context, caps ClaimCaps) (Deployment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Deployment{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, claimLockID); err != nil {
		return Deployment{}, err
	}
	bounded := caps.ByPlan != nil
	capExpr := `COALESCE((t.plan_limits->>'concurrent_builds')::int,
		CASE t.plan WHEN 'hobby' THEN $2::int WHEN 'pro' THEN $3::int ELSE $4::int END)`
	var id int64
	err = tx.QueryRowContext(ctx, `
		UPDATE deployments SET status = 'building', started_at = now(),
			attempts = attempts + 1, heartbeat_at = now()
		WHERE id = (
			SELECT d.id FROM deployments d
			JOIN apps a ON a.id = d.app_id JOIN teams t ON t.id = a.team_id
			WHERE d.status = 'queued' AND t.suspended_at IS NULL
				AND (NOT $1 OR `+capExpr+` < 0 OR (
					SELECT count(*) FROM deployments r JOIN apps ra ON ra.id = r.app_id
					WHERE ra.team_id = t.id AND r.status IN ('building', 'deploying')) < `+capExpr+`)
			ORDER BY d.id FOR UPDATE OF d SKIP LOCKED LIMIT 1
		)
		RETURNING id`, bounded, caps.plan(PlanHobby), caps.plan(PlanPro), caps.plan(PlanUnlimited)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Deployment{}, err
	}
	return s.GetDeployment(ctx, id)
}
