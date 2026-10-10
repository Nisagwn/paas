// Package notify delivers notifications (Faz 26): events of a team's apps
// (deployments, production changes, canary auto-rollbacks, add-on and
// domain problems, limits) go to the team's channels — email (SMTP),
// Slack and Discord incoming webhooks, and signed generic webhooks.
//
// Emitting is cheap and transactional: Service.Emit stores the event once
// per (team, type, key) and queues one delivery per matching channel in the
// outbox (store.EnqueueNotification). The Dispatcher sends the outbox in
// the background with retries and backoff, a per-channel rate limit and a
// stable idempotency key per event and channel (at-least-once delivery).
//
// Producers depend on the small Emitter interface only; main.go injects
// the Service. Deployment events come through DeploymentEvents, a
// worker.Notifier chained next to the GitHub notifier, so the worker's
// call sites stay as they are.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

// Event types a channel can subscribe to.
const (
	DeploymentReady    = "deployment.ready"
	DeploymentFailed   = "deployment.failed"
	DeploymentCanceled = "deployment.canceled"
	ProductionChanged  = "production.changed"
	RolloutRolledBack  = "rollout.rolled_back"
	AddonFailed        = "addon.failed"
	DomainFailed       = "domain.failed"
	CertExpiring       = "domain.certificate_expiring"
	LimitReached       = "limit.reached"
	// Test is sent by "Test gönder" to one channel; nobody subscribes to it.
	Test = "test"
)

// EventTypes lists the subscribable types in display order.
var EventTypes = []string{
	DeploymentReady, DeploymentFailed, DeploymentCanceled, ProductionChanged, RolloutRolledBack,
	AddonFailed, DomainFailed, CertExpiring, LimitReached,
}

// DefaultEvents is what a channel created without a list receives: every
// type except deployment.ready, which is frequent on busy teams.
var DefaultEvents = []string{
	DeploymentFailed, DeploymentCanceled, ProductionChanged, RolloutRolledBack,
	AddonFailed, DomainFailed, CertExpiring, LimitReached,
}

// EventLabels are the Turkish names of the event types (UI, emails).
var EventLabels = map[string]string{
	DeploymentReady:    "Deploy hazır",
	DeploymentFailed:   "Deploy başarısız",
	DeploymentCanceled: "Deploy iptal edildi",
	ProductionChanged:  "Production değişti (promote / geri alma)",
	RolloutRolledBack:  "Kademeli yayın otomatik geri alındı",
	AddonFailed:        "Veritabanı eklentisi hatası",
	DomainFailed:       "Alan adı doğrulanamadı",
	CertExpiring:       "Sertifikanın süresi doluyor",
	LimitReached:       "Limite ulaşıldı",
	Test:               "Test bildirimi",
}

// ValidEventType reports whether t is subscribable.
func ValidEventType(t string) bool {
	for _, e := range EventTypes {
		if e == t {
			return true
		}
	}
	return false
}

// Severities select the color of Slack / Discord messages.
const (
	SeveritySuccess = "success"
	SeverityInfo    = "info"
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// Field is a labeled value shown with the message.
type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Event is one notification. Producers fill the facts and a Turkish title
// and text (the constructors in this package do); Emit completes the rest.
type Event struct {
	Type string `json:"type"`
	// Key identifies the event among the team's events of this type: an
	// event emitted twice with the same key is stored and sent once.
	// Empty: unique per call.
	Key    string `json:"key"`
	TeamID int64  `json:"team_id"`
	Team   string `json:"team,omitempty"`
	// AppID 0 is a team-wide event (every channel of the team matches).
	AppID        int64   `json:"app_id,omitempty"`
	App          string  `json:"app,omitempty"`
	DeploymentID int64   `json:"deployment_id,omitempty"`
	Severity     string  `json:"severity"`
	Title        string  `json:"title"`
	Text         string  `json:"text,omitempty"`
	Fields       []Field `json:"fields,omitempty"`
	// Link is a path of the web UI (/deployments/12); Emit turns it into
	// URL with PAAS_PUBLIC_URL. LinkText labels the button.
	Link     string `json:"-"`
	URL      string `json:"url,omitempty"`
	LinkText string `json:"link_text,omitempty"`
	// ManageURL is the team's notification page (email footers).
	ManageURL string    `json:"manage_url,omitempty"`
	At        time.Time `json:"at"`
	// Data is machine-readable detail for webhook receivers.
	Data map[string]any `json:"data,omitempty"`
}

// Emitter accepts events. Implementations must be safe for concurrent use
// and must not block on delivery.
type Emitter interface {
	Emit(ctx context.Context, e Event) error
}

// Nop discards events.
type Nop struct{}

func (Nop) Emit(context.Context, Event) error { return nil }

// Service is the Emitter of the control plane: it stores events in the
// outbox and wakes the Dispatcher.
type Service struct {
	Store *store.Store
	// PublicURL is where the web UI is reachable (PAAS_PUBLIC_URL).
	PublicURL string
	// Dispatcher is optional: nil leaves deliveries queued (another replica
	// or a later start sends them).
	Dispatcher *Dispatcher
	// SMTPConfigured: email channels can be created.
	SMTPConfigured bool
	Validation     Validation
	Log            *slog.Logger
	Now            func() time.Time
}

var _ Emitter = (*Service)(nil)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// URL makes a web UI path absolute.
func (s *Service) URL(path string) string {
	if path == "" {
		return ""
	}
	return strings.TrimRight(s.PublicURL, "/") + path
}

// Emit stores e and queues its deliveries. The team comes from the app
// when only AppID is set.
func (s *Service) Emit(ctx context.Context, e Event) error {
	_, err := s.enqueue(ctx, e, 0)
	return err
}

func (s *Service) complete(ctx context.Context, e *Event) error {
	if e.Type == "" {
		return errors.New("notify: event without a type")
	}
	if e.TeamID == 0 && e.AppID != 0 {
		app, err := s.Store.GetAppByID(ctx, e.AppID)
		if err != nil {
			return fmt.Errorf("notify: app %d: %w", e.AppID, err)
		}
		e.TeamID, e.App = app.TeamID, app.Name
	}
	if e.TeamID == 0 {
		return errors.New("notify: event without a team")
	}
	if e.Team == "" {
		if t, err := s.Store.GetTeam(ctx, e.TeamID); err == nil {
			e.Team = t.Slug
		}
	}
	if e.At.IsZero() {
		e.At = s.now().UTC()
	}
	if e.Key == "" {
		e.Key = "t" + strconv.FormatInt(e.At.UnixNano(), 36)
	}
	if e.Severity == "" {
		e.Severity = SeverityInfo
	}
	if e.URL == "" && e.Link != "" && s.PublicURL != "" {
		e.URL = s.URL(e.Link)
	}
	if e.ManageURL == "" && e.Team != "" && s.PublicURL != "" {
		e.ManageURL = s.URL("/teams/" + e.Team + "/notifications")
	}
	return nil
}

func (s *Service) enqueue(ctx context.Context, e Event, only int64) (int64, error) {
	if err := s.complete(ctx, &e); err != nil {
		return 0, err
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	id, n, err := s.Store.EnqueueNotification(ctx, store.NotificationEventInput{
		TeamID: e.TeamID, AppID: e.AppID, Type: e.Type, Key: e.Key, Payload: payload, OnlyChannel: only,
	})
	if err != nil {
		return 0, fmt.Errorf("notify: %w", err)
	}
	if n > 0 {
		s.log().Info("notification queued", "type", e.Type, "team", e.Team, "app", e.App, "deliveries", n)
		if s.Dispatcher != nil {
			s.Dispatcher.Kick()
		}
	}
	return id, nil
}

// SendTest queues a test message for channel ch only and, with a
// Dispatcher, sends it at once. It returns the delivery as it stands
// afterwards (sent, failed, or pending for a retry / without dispatcher).
func (s *Service) SendTest(ctx context.Context, ch store.NotificationChannel, by string) (store.NotificationDelivery, error) {
	e := Event{
		Type: Test, TeamID: ch.TeamID, Severity: SeverityInfo,
		Title:  "Test bildirimi",
		Text:   fmt.Sprintf("Bu, %q bildirim kanalından gönderilen bir test mesajıdır. Bu mesajı görüyorsan kanal çalışıyor.", ch.Name),
		Fields: []Field{{Name: "Kanal", Value: ch.Name}, {Name: "Tür", Value: KindLabels[ch.Kind]}},
		Data:   map[string]any{"channel_id": ch.ID, "requested_by": by},
	}
	if by != "" {
		e.Fields = append(e.Fields, Field{Name: "Gönderen", Value: "@" + by})
	}
	if s.PublicURL != "" {
		e.LinkText = "Bildirim ayarları"
	}
	if err := s.complete(ctx, &e); err != nil {
		return store.NotificationDelivery{}, err
	}
	e.URL = e.ManageURL
	id, err := s.enqueue(ctx, e, ch.ID)
	if err != nil {
		return store.NotificationDelivery{}, err
	}
	ds, err := s.Store.DeliveriesOfEvent(ctx, id)
	if err != nil {
		return store.NotificationDelivery{}, err
	}
	if len(ds) == 0 {
		return store.NotificationDelivery{}, store.ErrNotFound
	}
	if s.Dispatcher != nil {
		s.Dispatcher.DeliverNow(ctx, ds[0].ID)
	}
	return s.Store.GetNotificationDelivery(ctx, ds[0].ID)
}

// KindLabels are the Turkish names of the channel kinds.
var KindLabels = map[string]string{
	store.ChannelEmail:   "E-posta",
	store.ChannelSlack:   "Slack",
	store.ChannelDiscord: "Discord",
	store.ChannelWebhook: "Webhook",
}
