package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Faz 26: notification channels, the event outbox and per-user email
// preferences (migration 019). internal/notify renders and sends; this
// file only persists.

// Channel kinds.
const (
	ChannelEmail   = "email"
	ChannelSlack   = "slack"
	ChannelDiscord = "discord"
	ChannelWebhook = "webhook"
)

// Delivery statuses.
const (
	DeliveryPending = "pending"
	DeliverySending = "sending"
	DeliverySent    = "sent"
	DeliveryFailed  = "failed"
	DeliverySkipped = "skipped"
)

// ValidChannelKind reports whether k is one of the four channel kinds.
func ValidChannelKind(k string) bool {
	return k == ChannelEmail || k == ChannelSlack || k == ChannelDiscord || k == ChannelWebhook
}

// NotificationChannel is a channel as listed; its URL and signing secret
// are never part of it (NotificationChannelSecrets).
type NotificationChannel struct {
	ID          int64    `json:"id"`
	TeamID      int64    `json:"team_id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Events      []string `json:"events"`
	Recipients  []string `json:"recipients,omitempty"`
	TeamMembers bool     `json:"team_members,omitempty"`
	// URLHint is scheme://host/… of the URL.
	URLHint string `json:"url_hint,omitempty"`
	// AppIDs / Apps narrow the channel; empty means every app of the team.
	AppIDs    []int64   `json:"app_ids"`
	Apps      []string  `json:"apps"`
	Enabled   bool      `json:"enabled"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Encrypted: the secrets are sealed with the env keyring.
	Encrypted bool `json:"encrypted"`
}

// ChannelSecrets are the sealed part of a channel.
type ChannelSecrets struct {
	URL           string `json:"url,omitempty"`
	SigningSecret string `json:"signing_secret,omitempty"`
}

// ChannelInput creates or replaces a channel's settings. Secrets nil on an
// update keeps the stored ones.
type ChannelInput struct {
	Name        string
	Kind        string
	Events      []string
	Recipients  []string
	TeamMembers bool
	AppIDs      []int64
	Enabled     bool
	Secrets     *ChannelSecrets
	URLHint     string
	CreatedBy   string
}

func notificationAAD(channelID int64) []byte {
	return []byte("paas/notification_channel\x00" + strconv.FormatInt(channelID, 10))
}

func (s *Store) sealChannel(id int64, sec ChannelSecrets) (string, sql.NullString, error) {
	if sec == (ChannelSecrets{}) {
		return "", sql.NullString{}, nil
	}
	raw, err := json.Marshal(sec)
	if err != nil {
		return "", sql.NullString{}, err
	}
	return s.seal(string(raw), notificationAAD(id))
}

func (s *Store) openChannel(id int64, stored string, keyID sql.NullString) (ChannelSecrets, error) {
	var sec ChannelSecrets
	if stored == "" {
		return sec, nil
	}
	plain, err := s.unseal(stored, keyID, notificationAAD(id))
	if err != nil {
		return sec, fmt.Errorf("notification channel %d: %w", id, err)
	}
	err = json.Unmarshal([]byte(plain), &sec)
	return sec, err
}

const channelCols = `c.id, c.team_id, c.name, c.kind, c.events, c.recipients, c.team_members, c.url_hint,
	c.enabled, c.created_by, c.created_at, c.updated_at, c.key_id IS NOT NULL,
	COALESCE((SELECT array_agg(ca.app_id ORDER BY a.name) FROM notification_channel_apps ca JOIN apps a ON a.id = ca.app_id
	          WHERE ca.channel_id = c.id), '{}'),
	COALESCE((SELECT array_agg(a.name ORDER BY a.name) FROM notification_channel_apps ca JOIN apps a ON a.id = ca.app_id
	          WHERE ca.channel_id = c.id), '{}')`

func scanChannel(row interface{ Scan(...any) error }) (NotificationChannel, error) {
	var c NotificationChannel
	var events, recipients, apps pq.StringArray
	var appIDs pq.Int64Array
	err := row.Scan(&c.ID, &c.TeamID, &c.Name, &c.Kind, &events, &recipients, &c.TeamMembers, &c.URLHint,
		&c.Enabled, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.Encrypted, &appIDs, &apps)
	c.Events, c.Recipients, c.AppIDs, c.Apps = notifyStrings(events), notifyStrings(recipients), []int64(appIDs), notifyStrings(apps)
	if c.AppIDs == nil {
		c.AppIDs = []int64{}
	}
	return c, err
}

func notifyStrings(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

// ListNotificationChannels returns the team's channels by name.
func (s *Store) ListNotificationChannels(ctx context.Context, teamID int64) ([]NotificationChannel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelCols+` FROM notification_channels c
		WHERE c.team_id = $1 ORDER BY c.name`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotificationChannel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetNotificationChannel returns channel id of the team (teamID 0: any team).
func (s *Store) GetNotificationChannel(ctx context.Context, teamID, id int64) (NotificationChannel, error) {
	c, err := scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelCols+` FROM notification_channels c
		WHERE c.id = $1 AND ($2 = 0 OR c.team_id = $2)`, id, teamID))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// NotificationChannelSecrets decrypts the channel's URL and signing secret.
func (s *Store) NotificationChannelSecrets(ctx context.Context, id int64) (ChannelSecrets, error) {
	var stored string
	var keyID sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT secrets, key_id FROM notification_channels WHERE id = $1`, id).Scan(&stored, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return ChannelSecrets{}, ErrNotFound
	}
	if err != nil {
		return ChannelSecrets{}, err
	}
	return s.openChannel(id, stored, keyID)
}

// CreateNotificationChannel adds a channel to the team. App ids that are
// not the team's are ignored. ErrConflict: the name is taken.
func (s *Store) CreateNotificationChannel(ctx context.Context, teamID int64, in ChannelInput) (NotificationChannel, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NotificationChannel{}, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO notification_channels (team_id, name, kind, events, recipients, team_members, url_hint, enabled, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		teamID, in.Name, in.Kind, pq.Array(notifyStrings(in.Events)), pq.Array(notifyStrings(in.Recipients)), in.TeamMembers,
		in.URLHint, in.Enabled, in.CreatedBy).Scan(&id)
	if isUniqueViolation(err) {
		return NotificationChannel{}, ErrConflict
	}
	if err != nil {
		return NotificationChannel{}, err
	}
	if in.Secrets != nil {
		if err := s.storeChannelSecrets(ctx, tx, id, *in.Secrets); err != nil {
			return NotificationChannel{}, err
		}
	}
	if err := setChannelApps(ctx, tx, teamID, id, in.AppIDs); err != nil {
		return NotificationChannel{}, err
	}
	if err := tx.Commit(); err != nil {
		return NotificationChannel{}, err
	}
	return s.GetNotificationChannel(ctx, teamID, id)
}

// UpdateNotificationChannel replaces the settings of channel id of the
// team; the kind never changes. ErrNotFound, ErrConflict (name taken).
func (s *Store) UpdateNotificationChannel(ctx context.Context, teamID, id int64, in ChannelInput) (NotificationChannel, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NotificationChannel{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE notification_channels SET name = $3, events = $4, recipients = $5, team_members = $6, enabled = $7,
			url_hint = CASE WHEN $8 = '' THEN url_hint ELSE $8 END, updated_at = now()
		WHERE id = $1 AND team_id = $2`,
		id, teamID, in.Name, pq.Array(notifyStrings(in.Events)), pq.Array(notifyStrings(in.Recipients)), in.TeamMembers, in.Enabled, in.URLHint)
	if isUniqueViolation(err) {
		return NotificationChannel{}, ErrConflict
	}
	if err != nil {
		return NotificationChannel{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return NotificationChannel{}, ErrNotFound
	}
	if in.Secrets != nil {
		if err := s.storeChannelSecrets(ctx, tx, id, *in.Secrets); err != nil {
			return NotificationChannel{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM notification_channel_apps WHERE channel_id = $1`, id); err != nil {
		return NotificationChannel{}, err
	}
	if err := setChannelApps(ctx, tx, teamID, id, in.AppIDs); err != nil {
		return NotificationChannel{}, err
	}
	if err := tx.Commit(); err != nil {
		return NotificationChannel{}, err
	}
	return s.GetNotificationChannel(ctx, teamID, id)
}

func (s *Store) storeChannelSecrets(ctx context.Context, tx *sql.Tx, id int64, sec ChannelSecrets) error {
	stored, keyID, err := s.sealChannel(id, sec)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE notification_channels SET secrets = $2, key_id = $3 WHERE id = $1`, id, stored, keyID)
	return err
}

func setChannelApps(ctx context.Context, tx *sql.Tx, teamID, id int64, appIDs []int64) error {
	if len(appIDs) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO notification_channel_apps (channel_id, app_id)
		SELECT $1, a.id FROM apps a WHERE a.team_id = $2 AND a.id = ANY($3)
		ON CONFLICT DO NOTHING`, id, teamID, pq.Array(appIDs))
	return err
}

// DeleteNotificationChannel removes the channel and its delivery log.
func (s *Store) DeleteNotificationChannel(ctx context.Context, teamID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM notification_channels WHERE id = $1 AND team_id = $2`, id, teamID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- events and the outbox ----

// NotificationEventInput is an event to store and fan out.
type NotificationEventInput struct {
	TeamID int64
	// AppID 0: a team-wide event (every channel of the team matches).
	AppID   int64
	Type    string
	Key     string
	Payload []byte
	// OnlyChannel: deliver to this channel only, whatever it subscribes to
	// and even when disabled (test messages).
	OnlyChannel int64
}

// EnqueueNotification stores the event and one pending delivery per
// matching channel, in one transaction. An event already stored under the
// same (team, type, key) is not stored again: eventID is 0 and nothing is
// queued, which makes emitting idempotent.
func (s *Store) EnqueueNotification(ctx context.Context, e NotificationEventInput) (eventID int64, queued int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var app sql.NullInt64
	if e.AppID != 0 {
		app = sql.NullInt64{Int64: e.AppID, Valid: true}
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO notification_events (team_id, app_id, type, event_key, payload) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (team_id, type, event_key) DO NOTHING RETURNING id`,
		e.TeamID, app, e.Type, e.Key, string(e.Payload)).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil // emitted before
	}
	if err != nil {
		return 0, 0, err
	}
	// The idempotency key is derived from the event's identity and the
	// channel, never from row ids, so it is stable for receivers.
	const keyExpr = `left(encode(sha256(convert_to(c.team_id::text || ':' || $3::text || ':' || $4 || ':' || c.id::text, 'UTF8')), 'hex'), 40)`
	var res sql.Result
	if e.OnlyChannel != 0 {
		res, err = tx.ExecContext(ctx, `
			INSERT INTO notification_deliveries (event_id, channel_id, idempotency_key)
			SELECT $1, c.id, `+keyExpr+` FROM notification_channels c WHERE c.id = $5 AND c.team_id = $2
			ON CONFLICT DO NOTHING`, eventID, e.TeamID, e.Type, e.Key, e.OnlyChannel)
	} else {
		res, err = tx.ExecContext(ctx, `
			INSERT INTO notification_deliveries (event_id, channel_id, idempotency_key)
			SELECT $1, c.id, `+keyExpr+` FROM notification_channels c
			WHERE c.team_id = $2 AND c.enabled AND $3 = ANY(c.events)
			  AND ($5::bigint IS NULL
			       OR NOT EXISTS (SELECT 1 FROM notification_channel_apps ca WHERE ca.channel_id = c.id)
			       OR EXISTS (SELECT 1 FROM notification_channel_apps ca WHERE ca.channel_id = c.id AND ca.app_id = $5))
			ON CONFLICT DO NOTHING`, eventID, e.TeamID, e.Type, e.Key, app)
	}
	if err != nil {
		return 0, 0, err
	}
	n, _ := res.RowsAffected()
	return eventID, int(n), tx.Commit()
}

// NotificationJob is a claimed delivery with what sending it needs.
type NotificationJob struct {
	DeliveryID     int64
	IdempotencyKey string
	Attempts       int
	EventID        int64
	EventType      string
	EventKey       string
	Payload        []byte
	EventAt        time.Time
	Channel        NotificationChannel
}

// ClaimNotificationDeliveries leases up to limit due deliveries: pending
// ones whose next attempt is due and sending ones whose lease ran out (a
// dispatcher died mid-send). Each claim counts as an attempt. Rows are
// locked with SKIP LOCKED, so several dispatchers share the outbox. only
// != 0 claims that delivery only: if it is due, or new (a test message is
// sent at once even when the database clock runs ahead of now).
func (s *Store) ClaimNotificationDeliveries(ctx context.Context, now time.Time, limit int, lease time.Duration, only int64) ([]NotificationJob, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH claimed AS (
			UPDATE notification_deliveries d SET status = 'sending', attempts = d.attempts + 1,
				locked_until = $1::timestamptz + make_interval(secs => $3), updated_at = $1
			WHERE d.id IN (
				SELECT id FROM notification_deliveries
				WHERE ((status = 'pending' AND (next_attempt_at <= $1 OR ($4 <> 0 AND attempts = 0)))
				       OR (status = 'sending' AND locked_until < $1))
				  AND ($4 = 0 OR id = $4)
				ORDER BY next_attempt_at, id LIMIT $2 FOR UPDATE SKIP LOCKED)
			RETURNING d.id, d.idempotency_key, d.attempts, d.event_id, d.channel_id)
		SELECT cl.id, cl.idempotency_key, cl.attempts, e.id, e.type, e.event_key, e.payload, e.created_at, `+channelCols+`
		FROM claimed cl
		JOIN notification_events e ON e.id = cl.event_id
		JOIN notification_channels c ON c.id = cl.channel_id
		ORDER BY cl.id`, now, limit, lease.Seconds(), only)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationJob
	for rows.Next() {
		var j NotificationJob
		var payload string
		var c NotificationChannel
		var events, recipients, apps pq.StringArray
		var appIDs pq.Int64Array
		if err := rows.Scan(&j.DeliveryID, &j.IdempotencyKey, &j.Attempts, &j.EventID, &j.EventType, &j.EventKey, &payload, &j.EventAt,
			&c.ID, &c.TeamID, &c.Name, &c.Kind, &events, &recipients, &c.TeamMembers, &c.URLHint,
			&c.Enabled, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.Encrypted, &appIDs, &apps); err != nil {
			return nil, err
		}
		c.Events, c.Recipients, c.AppIDs, c.Apps = notifyStrings(events), notifyStrings(recipients), []int64(appIDs), notifyStrings(apps)
		j.Payload, j.Channel = []byte(payload), c
		out = append(out, j)
	}
	return out, rows.Err()
}

// DeliveryOutcome records the result of one attempt.
type DeliveryOutcome struct {
	// Status: sent, failed (final), skipped, or pending (retry at NextAttempt).
	Status      string
	Code        int
	Error       string
	NextAttempt time.Time
	// Refund: the attempt does not count (rate limited before sending).
	Refund bool
}

// FinishNotificationDelivery stores the outcome of the attempt that
// claimed delivery id. It is a no-op if the lease was lost meanwhile.
func (s *Store) FinishNotificationDelivery(ctx context.Context, id int64, now time.Time, o DeliveryOutcome) error {
	var code sql.NullInt64
	if o.Code != 0 {
		code = sql.NullInt64{Int64: int64(o.Code), Valid: true}
	}
	next := o.NextAttempt
	if next.IsZero() {
		next = now
	}
	refund := 0
	if o.Refund {
		refund = 1
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE notification_deliveries SET status = $2,
			response_code = CASE WHEN $3::int IS NULL THEN response_code ELSE $3 END,
			last_error = $4, next_attempt_at = $5, locked_until = NULL, updated_at = $6,
			sent_at = CASE WHEN $2 = 'sent' THEN $6 ELSE sent_at END,
			attempts = GREATEST(attempts - $7, 0)
		WHERE id = $1 AND status = 'sending'`, id, o.Status, code, truncateNotify(o.Error, 1000), next, now, refund)
	return err
}

// NotificationsSentSince counts the channel's deliveries sent after since
// (rate limiting).
func (s *Store) NotificationsSentSince(ctx context.Context, channelID int64, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries
		WHERE channel_id = $1 AND status = 'sent' AND sent_at > $2`, channelID, since).Scan(&n)
	return n, err
}

// NotificationDelivery is a row of a channel's delivery log.
type NotificationDelivery struct {
	ID             int64           `json:"id"`
	ChannelID      int64           `json:"channel_id"`
	EventID        int64           `json:"event_id"`
	EventType      string          `json:"event_type"`
	Payload        json.RawMessage `json:"event"`
	IdempotencyKey string          `json:"idempotency_key"`
	Status         string          `json:"status"`
	Attempts       int             `json:"attempts"`
	ResponseCode   *int            `json:"response_code,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	NextAttemptAt  time.Time       `json:"next_attempt_at"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	SentAt         *time.Time      `json:"sent_at,omitempty"`
}

const deliveryCols = `d.id, d.channel_id, d.event_id, e.type, e.payload, d.idempotency_key, d.status, d.attempts,
	d.response_code, d.last_error, d.next_attempt_at, d.created_at, d.updated_at, d.sent_at`

func scanDelivery(row interface{ Scan(...any) error }) (NotificationDelivery, error) {
	var d NotificationDelivery
	var payload string
	var code sql.NullInt64
	err := row.Scan(&d.ID, &d.ChannelID, &d.EventID, &d.EventType, &payload, &d.IdempotencyKey, &d.Status, &d.Attempts,
		&code, &d.LastError, &d.NextAttemptAt, &d.CreatedAt, &d.UpdatedAt, &d.SentAt)
	d.Payload = json.RawMessage(payload)
	if code.Valid {
		c := int(code.Int64)
		d.ResponseCode = &c
	}
	return d, err
}

// ListNotificationDeliveries returns the channel's latest deliveries.
func (s *Store) ListNotificationDeliveries(ctx context.Context, channelID int64, limit int) ([]NotificationDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM notification_deliveries d
		JOIN notification_events e ON e.id = d.event_id
		WHERE d.channel_id = $1 ORDER BY d.id DESC LIMIT $2`, channelID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotificationDelivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetNotificationDelivery returns one delivery.
func (s *Store) GetNotificationDelivery(ctx context.Context, id int64) (NotificationDelivery, error) {
	d, err := scanDelivery(s.db.QueryRowContext(ctx, `SELECT `+deliveryCols+` FROM notification_deliveries d
		JOIN notification_events e ON e.id = d.event_id WHERE d.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// DeliveriesOfEvent returns the deliveries queued for an event.
func (s *Store) DeliveriesOfEvent(ctx context.Context, eventID int64) ([]NotificationDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM notification_deliveries d
		JOIN notification_events e ON e.id = d.event_id WHERE d.event_id = $1 ORDER BY d.id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationDelivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PruneNotifications deletes events older than before whose deliveries
// are all finished (with their deliveries) and returns how many.
func (s *Store) PruneNotifications(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM notification_events e WHERE e.created_at < $1
		AND NOT EXISTS (SELECT 1 FROM notification_deliveries d WHERE d.event_id = e.id AND d.status IN ('pending', 'sending'))`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---- per-user email preferences ----

// NotificationPrefs are a user's email preferences.
type NotificationPrefs struct {
	UserID       int64     `json:"-"`
	Email        string    `json:"email"`
	EmailEnabled bool      `json:"email_enabled"`
	MutedEvents  []string  `json:"muted_events"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

// Wants reports whether the preferences allow an email about eventType.
// Test messages always pass: the user asked for one.
func (p NotificationPrefs) Wants(eventType string) bool {
	if !p.EmailEnabled {
		return false
	}
	for _, m := range p.MutedEvents {
		if m == eventType {
			return false
		}
	}
	return true
}

// GetNotificationPrefs returns the user's preferences (defaults when none
// were saved: no address, email on, nothing muted).
func (s *Store) GetNotificationPrefs(ctx context.Context, userID int64) (NotificationPrefs, error) {
	p := NotificationPrefs{UserID: userID, EmailEnabled: true, MutedEvents: []string{}}
	var muted pq.StringArray
	err := s.db.QueryRowContext(ctx, `SELECT email, email_enabled, muted_events, updated_at
		FROM notification_preferences WHERE user_id = $1`, userID).Scan(&p.Email, &p.EmailEnabled, &muted, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	p.MutedEvents = notifyStrings(muted)
	return p, err
}

// SetNotificationPrefs saves the user's preferences.
func (s *Store) SetNotificationPrefs(ctx context.Context, p NotificationPrefs) (NotificationPrefs, error) {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_preferences (user_id, email, email_enabled, muted_events, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (user_id) DO UPDATE SET email = $2, email_enabled = $3, muted_events = $4, updated_at = now()`,
		p.UserID, p.Email, p.EmailEnabled, pq.Array(notifyStrings(p.MutedEvents)))
	if err != nil {
		return p, err
	}
	return s.GetNotificationPrefs(ctx, p.UserID)
}

// NotificationEmailRecipients resolves the addresses an email channel
// sends eventType to: its explicit addresses and, with team_members, the
// addresses team members saved. A user's preferences apply to every
// address they saved, listed explicitly or not. eventType "test" ignores
// the opt-outs. Addresses are deduplicated case-insensitively, sorted.
func (s *Store) NotificationEmailRecipients(ctx context.Context, ch NotificationChannel, eventType string) ([]string, error) {
	type pref struct {
		enabled bool
		muted   []string
	}
	prefs := map[string]pref{}
	var members []string
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.email, p.email_enabled, p.muted_events,
			EXISTS (SELECT 1 FROM team_members m WHERE m.team_id = $1 AND m.user_id = p.user_id)
		FROM notification_preferences p WHERE p.email <> ''`, ch.TeamID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var email string
		var enabled, member bool
		var muted pq.StringArray
		if err := rows.Scan(&email, &enabled, &muted, &member); err != nil {
			rows.Close()
			return nil, err
		}
		key := strings.ToLower(email)
		// The same address saved by several users: any opt-out wins.
		if p, ok := prefs[key]; ok {
			enabled = enabled && p.enabled
			muted = append(muted, p.muted...)
		}
		prefs[key] = pref{enabled, muted}
		if member {
			members = append(members, email)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	candidates := append([]string{}, ch.Recipients...)
	if ch.TeamMembers {
		candidates = append(candidates, members...)
	}
	seen := map[string]bool{}
	var out []string
	for _, addr := range candidates {
		key := strings.ToLower(strings.TrimSpace(addr))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if p, ok := prefs[key]; ok && eventType != "test" {
			if !(NotificationPrefs{EmailEnabled: p.enabled, MutedEvents: p.muted}).Wants(eventType) {
				continue
			}
		}
		out = append(out, strings.TrimSpace(addr))
	}
	sort.Strings(out)
	return out, nil
}

// ---- key rotation (envcrypt.go) ----

const notificationKeyStatsQuery = `
	SELECT COALESCE(key_id, ''), count(*) FROM notification_channels WHERE secrets <> '' GROUP BY 1`

// reencryptNotifications seals channel secrets under the current key
// inside tx; see ReencryptEnv.
func (s *Store) reencryptNotifications(ctx context.Context, tx *sql.Tx) (int, error) {
	cur := s.env.CurrentID()
	type row struct {
		id     int64
		stored string
		keyID  sql.NullString
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, secrets, key_id FROM notification_channels
		WHERE secrets <> '' AND key_id IS DISTINCT FROM $1 ORDER BY id FOR UPDATE`, cur)
	if err != nil {
		return 0, err
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.stored, &r.keyID); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range todo {
		sec, err := s.openChannel(r.id, r.stored, r.keyID)
		if err != nil {
			return 0, err
		}
		if err := s.storeChannelSecrets(ctx, tx, r.id, sec); err != nil {
			return 0, err
		}
	}
	return len(todo), nil
}

func truncateNotify(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !notifyRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func notifyRuneStart(b byte) bool { return b&0xC0 != 0x80 }
