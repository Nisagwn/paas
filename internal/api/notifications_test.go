package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
)

// notificationChannel creates a Slack channel of team "web".
func (f *teamFixture) notificationChannel(name string) store.NotificationChannel {
	f.t.Helper()
	ch, err := f.st.CreateNotificationChannel(context.Background(), f.web.ID, store.ChannelInput{
		Name: name, Kind: store.ChannelSlack, Events: notify.DefaultEvents, Enabled: true,
		Secrets: &store.ChannelSecrets{URL: "https://hooks.slack.com/services/T/B/secret"}, URLHint: "https://hooks.slack.com/…",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return ch
}

// TestNotificationChannelsAPI: secrets are shown once and never returned;
// invalid input is 400; a test message is sent through the dispatcher and
// shows up in the delivery log; rollback emits production.changed.
func TestNotificationChannelsAPI(t *testing.T) {
	f := setupTeams(t)
	ctx := context.Background()

	var mu sync.Mutex
	var got []*http.Request
	var bodies []string
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, bodies = append(got, r), append(bodies, string(b))
		mu.Unlock()
	}))
	defer rcv.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := notify.NewHTTPClient(0, true)
	// Rows are queued at the database's now(); the dispatcher's clock runs
	// a minute ahead so a skew between the clocks does not matter.
	disp := &notify.Dispatcher{Store: f.st, Log: log, Now: func() time.Time { return time.Now().Add(time.Minute) }, Senders: map[string]notify.Sender{
		store.ChannelWebhook: notify.WebhookSender{Client: client},
		store.ChannelSlack:   notify.SlackSender{Client: client},
	}}
	svc := &notify.Service{Store: f.st, PublicURL: "https://paas.example.com", Dispatcher: disp, Log: log,
		Validation: notify.Validation{AllowInsecureURLs: true}}
	srv := httptest.NewServer((&api.Server{
		Store: f.st, Domain: domain, APIToken: token, WebhookSecret: secret, Log: log, Sessions: f.ss,
		Notifications: svc,
	}).Handler())
	defer srv.Close()
	f.srv = srv

	// Invalid input.
	for _, body := range []map[string]any{
		{"name": "x", "kind": "sms", "url": rcv.URL},
		{"name": "x", "kind": "webhook"},
		{"name": "x", "kind": "webhook", "url": "ftp://example.com"},
		{"name": "x", "kind": "webhook", "url": rcv.URL, "events": []string{"nope"}},
		{"name": "x", "kind": "webhook", "url": rcv.URL, "apps": []string{"infra"}}, // another team's app
		{"name": "x", "kind": "email", "recipients": []string{"a@example.com"}},     // no SMTP configured
	} {
		if code, b := f.call("bob", "POST", "/api/teams/web/notifications", body); code != 400 {
			t.Fatalf("%v: %d %s", body, code, b)
		}
	}

	code, b := f.call("bob", "POST", "/api/teams/web/notifications", map[string]any{
		"name": "hook", "kind": "webhook", "url": rcv.URL + "/secret-path", "apps": []string{"blog"},
		"events": []string{notify.DeploymentFailed, notify.ProductionChanged},
	})
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	var created struct {
		store.NotificationChannel
		SigningSecret string `json:"signing_secret"`
	}
	json.Unmarshal([]byte(b), &created)
	if !strings.HasPrefix(created.SigningSecret, "whsec_") || strings.Contains(b, "secret-path") {
		t.Fatalf("create response: %s", b)
	}
	path := fmt.Sprintf("/api/teams/web/notifications/%d", created.ID)
	for _, p := range []string{path, "/api/teams/web/notifications"} {
		_, b := f.call("carol", "GET", p, nil)
		if strings.Contains(b, created.SigningSecret) || strings.Contains(b, "secret-path") || strings.Contains(b, "whsec_") {
			t.Fatalf("%s leaks a secret: %s", p, b)
		}
	}
	// Changing the events keeps the URL and the signing secret.
	if code, b := f.call("bob", "PUT", path, map[string]any{"events": []string{notify.DeploymentFailed, notify.ProductionChanged, notify.DeploymentReady}}); code != 200 || strings.Contains(b, "whsec_") {
		t.Fatalf("update: %d %s", code, b)
	}
	if code, b := f.call("bob", "PUT", path, map[string]any{"kind": "slack"}); code != 400 {
		t.Fatalf("kind change: %d %s", code, b)
	}

	// Test message: sent at once, signed with the secret shown at creation.
	code, b = f.call("bob", "POST", path+"/test", nil)
	if code != 200 || !strings.Contains(b, `"status":"sent"`) {
		t.Fatalf("test: %d %s", code, b)
	}
	mu.Lock()
	if len(got) != 1 || got[0].URL.Path != "/secret-path" || got[0].Header.Get(notify.HeaderEvent) != notify.Test ||
		notify.VerifySignature(created.SigningSecret, got[0].Header, []byte(bodies[0]), nowFor(got[0]), 0) != nil {
		mu.Unlock()
		t.Fatalf("test request: %d %v", len(got), got)
	}
	mu.Unlock()

	// Rollback through the API emits production.changed for the channel.
	if code, b := f.call("bob", "POST", "/api/apps/blog/rollback", map[string]int64{"deployment_id": f.d1.ID}); code != 200 {
		t.Fatalf("rollback: %d %s", code, b)
	}
	disp.Tick(ctx)
	code, b = f.call("carol", "GET", path+"/deliveries", nil)
	var ds []store.NotificationDelivery
	json.Unmarshal([]byte(b), &ds)
	if code != 200 || len(ds) != 2 || ds[0].EventType != notify.ProductionChanged || ds[0].Status != store.DeliverySent ||
		ds[1].EventType != notify.Test {
		t.Fatalf("deliveries: %d %s", code, b)
	}
	var e notify.Event
	json.Unmarshal(ds[0].Payload, &e)
	if e.DeploymentID != f.d1.ID || e.URL != fmt.Sprintf("https://paas.example.com/deployments/%d", f.d1.ID) ||
		!strings.Contains(e.Text, "geri alındı") {
		t.Fatalf("production event: %+v", e)
	}

	// Preferences.
	if code, b := f.call("bob", "PUT", "/api/me/notifications", map[string]any{"email": "bob@example.com", "muted_events": []string{"nope"}}); code != 400 {
		t.Fatalf("bad prefs: %d %s", code, b)
	}
	if code, b := f.call("bob", "PUT", "/api/me/notifications", map[string]any{"email": "bob@example.com", "email_enabled": false}); code != 200 ||
		!strings.Contains(b, `"email_enabled":false`) {
		t.Fatalf("prefs: %d %s", code, b)
	}

	if code, _ := f.call("bob", "DELETE", path, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := f.call("bob", "GET", path, nil); code != 404 {
		t.Fatalf("deleted channel: %d", code)
	}
}

// nowFor is the time a webhook request was signed (its timestamp header),
// so VerifySignature with tolerance 0 checks the signature only.
func nowFor(r *http.Request) time.Time {
	var ts int64
	fmt.Sscan(r.Header.Get(notify.HeaderTimestamp), &ts)
	return time.Unix(ts, 0)
}
