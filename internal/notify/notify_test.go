package notify_test

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/secret"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

func failedEvent(f *fixture, key string) notify.Event {
	return notify.Event{Type: notify.DeploymentFailed, Key: key, AppID: f.blog.ID, Severity: notify.SeverityError,
		Title: "blog: deploy başarısız", Text: "Hata", Link: "/deployments/7"}
}

// TestOutboxRetriesAndIdempotency: one delivery per event and channel even
// when emitted twice; failures back off exponentially with the same
// idempotency key and a valid signature; the delivery ends sent.
func TestOutboxRetriesAndIdempotency(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	rcv := newReceiver(t, 500, 503)
	ch, signing := f.channel(notify.ChannelRequest{Name: "hook", Kind: store.ChannelWebhook, URL: rcv.URL})
	if !strings.HasPrefix(signing, "whsec_") {
		t.Fatalf("signing secret %q", signing)
	}
	for i := 0; i < 2; i++ {
		if err := f.svc.Emit(ctx, failedEvent(f, "deployment:7:failed")); err != nil {
			t.Fatal(err)
		}
	}
	if ds := f.deliveries(ch); len(ds) != 1 {
		t.Fatalf("idempotent emit: %d deliveries", len(ds))
	}

	if n := f.disp.Tick(ctx); n != 1 {
		t.Fatalf("first tick claimed %d", n)
	}
	d := f.deliveries(ch)[0]
	if d.Status != store.DeliveryPending || d.Attempts != 1 || d.ResponseCode == nil || *d.ResponseCode != 500 ||
		d.NextAttemptAt.Sub(f.clock.Now().Add(30*time.Second)).Abs() > time.Millisecond {
		t.Fatalf("after 500: %+v (now %v)", d, f.clock.Now())
	}
	if n := f.disp.Tick(ctx); n != 0 {
		t.Fatal("retried before the backoff")
	}
	f.clock.Add(31 * time.Second)
	f.disp.Tick(ctx)
	d = f.deliveries(ch)[0]
	if d.Status != store.DeliveryPending || d.Attempts != 2 || !d.NextAttemptAt.After(f.clock.Now().Add(59*time.Second)) {
		t.Fatalf("second backoff should double: %+v", d)
	}
	f.clock.Add(61 * time.Second)
	f.disp.Tick(ctx)
	d = f.deliveries(ch)[0]
	if d.Status != store.DeliverySent || d.Attempts != 3 || d.SentAt == nil {
		t.Fatalf("after success: %+v", d)
	}
	if rcv.count() != 3 {
		t.Fatalf("receiver got %d requests", rcv.count())
	}
	for i := 0; i < 3; i++ {
		h := rcv.headers[i]
		if h.Get(notify.HeaderDelivery) != d.IdempotencyKey || h.Get("Idempotency-Key") != d.IdempotencyKey {
			t.Fatalf("attempt %d: delivery id %q, want %q", i, h.Get(notify.HeaderDelivery), d.IdempotencyKey)
		}
		if err := notify.VerifySignature(signing, h, rcv.bodies[i], f.clock.Now(), 5*time.Minute); err != nil {
			t.Fatalf("attempt %d signature: %v", i, err)
		}
		if h.Get(notify.HeaderEvent) != notify.DeploymentFailed {
			t.Fatalf("event header %q", h.Get(notify.HeaderEvent))
		}
	}
	if err := notify.VerifySignature("wrong", rcv.headers[0], rcv.bodies[0], f.clock.Now(), 5*time.Minute); err == nil {
		t.Fatal("wrong secret verified")
	}
	var body notify.WebhookBody
	json.Unmarshal(rcv.bodies[2], &body)
	if body.ID != d.IdempotencyKey || body.App != "blog" || body.Team != "web" || body.URL != "https://paas.example.com/deployments/7" {
		t.Fatalf("webhook body: %+v", body)
	}
	// A new key is a new event.
	f.svc.Emit(ctx, failedEvent(f, "deployment:8:failed"))
	if ds := f.deliveries(ch); len(ds) != 2 || ds[0].IdempotencyKey == d.IdempotencyKey {
		t.Fatalf("second event: %+v", ds)
	}
}

func TestPermanentFailuresAndGivingUp(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	bad := newReceiver(t, 404)
	down := newReceiver(t, 500, 500, 500, 500, 500)
	chBad, _ := f.channel(notify.ChannelRequest{Name: "bad", Kind: store.ChannelSlack, URL: bad.URL})
	chDown, _ := f.channel(notify.ChannelRequest{Name: "down", Kind: store.ChannelDiscord, URL: down.URL})
	f.svc.Emit(ctx, failedEvent(f, "k1"))
	f.disp.Tick(ctx)
	if d := f.deliveries(chBad)[0]; d.Status != store.DeliveryFailed || d.Attempts != 1 {
		t.Fatalf("404 is permanent: %+v", d)
	}
	for i := 0; i < 6; i++ {
		f.clock.Add(time.Hour)
		f.disp.Tick(ctx)
	}
	d := f.deliveries(chDown)[0]
	if d.Status != store.DeliveryFailed || d.Attempts != 4 || !strings.Contains(d.LastError, "vazgeçildi") {
		t.Fatalf("max attempts: %+v", d)
	}
	if down.count() != 4 {
		t.Fatalf("down got %d requests", down.count())
	}
}

func TestRateLimitAndRetryAfter(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.disp.RateLimit = 2
	rcv := newReceiver(t)
	ch, _ := f.channel(notify.ChannelRequest{Name: "s", Kind: store.ChannelSlack, URL: rcv.URL})
	for _, k := range []string{"a", "b", "c"} {
		f.svc.Emit(ctx, failedEvent(f, k))
	}
	f.disp.Parallel = 1
	f.disp.Tick(ctx)
	sent, waiting := 0, 0
	for _, d := range f.deliveries(ch) {
		switch d.Status {
		case store.DeliverySent:
			sent++
		case store.DeliveryPending:
			waiting++
			if d.Attempts != 0 || !strings.Contains(d.LastError, "hız sınırı") {
				t.Fatalf("rate limited delivery: %+v", d)
			}
		}
	}
	if sent != 2 || waiting != 1 {
		t.Fatalf("sent %d waiting %d", sent, waiting)
	}

	// 429 with Retry-After: the next attempt waits at least that long.
	f.disp.RateLimit = 0
	r429 := newReceiver(t, 429)
	r429.retry = "120"
	ch2, _ := f.channel(notify.ChannelRequest{Name: "d", Kind: store.ChannelDiscord, URL: r429.URL})
	f.svc.Emit(ctx, failedEvent(f, "d"))
	f.disp.Tick(ctx)
	d := f.deliveries(ch2)[0]
	if d.Status != store.DeliveryPending || d.NextAttemptAt.Before(f.clock.Now().Add(119*time.Second)) {
		t.Fatalf("retry-after: %+v", d)
	}
}

func TestLeaseExpiryReclaims(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	rcv := newReceiver(t)
	ch, _ := f.channel(notify.ChannelRequest{Name: "s", Kind: store.ChannelSlack, URL: rcv.URL})
	f.svc.Emit(ctx, failedEvent(f, "x"))
	// A dispatcher claims and dies.
	jobs, err := f.st.ClaimNotificationDeliveries(ctx, f.clock.Now(), 10, time.Minute, 0)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v %d", err, len(jobs))
	}
	if n := f.disp.Tick(ctx); n != 0 {
		t.Fatal("leased delivery claimed twice")
	}
	f.clock.Add(2 * time.Minute)
	if n := f.disp.Tick(ctx); n != 1 {
		t.Fatal("expired lease not reclaimed")
	}
	if d := f.deliveries(ch)[0]; d.Status != store.DeliverySent || d.Attempts != 2 {
		t.Fatalf("reclaimed: %+v", d)
	}
}

func TestSubscriptionsAndAppFilter(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	rcv := newReceiver(t)
	all, _ := f.channel(notify.ChannelRequest{Name: "all", Kind: store.ChannelSlack, URL: rcv.URL})
	onlyShop, _ := f.channel(notify.ChannelRequest{Name: "shop", Kind: store.ChannelSlack, URL: rcv.URL, Apps: strs("shop")})
	readyOnly, _ := f.channel(notify.ChannelRequest{Name: "ready", Kind: store.ChannelSlack, URL: rcv.URL, Events: strs(notify.DeploymentReady)})
	off := false
	disabled, _ := f.channel(notify.ChannelRequest{Name: "off", Kind: store.ChannelSlack, URL: rcv.URL, Enabled: &off})
	if !strings.Contains(strings.Join(all.Events, ","), notify.DeploymentFailed) || strings.Contains(strings.Join(all.Events, ","), notify.DeploymentReady) {
		t.Fatalf("default events: %v", all.Events)
	}
	if len(onlyShop.Apps) != 1 || onlyShop.Apps[0] != "shop" {
		t.Fatalf("apps: %+v", onlyShop)
	}
	f.svc.Emit(ctx, failedEvent(f, "1")) // blog, failed
	// Team-wide event: every subscribed channel, narrowed or not.
	f.svc.Emit(ctx, notify.Limit(f.team.ID, 0, "", "apps", 3, 3, "", f.clock.Now()))
	got := map[string]int{}
	for _, ch := range []store.NotificationChannel{all, onlyShop, readyOnly, disabled} {
		got[ch.Name] = len(f.deliveries(ch))
	}
	if got["all"] != 2 || got["shop"] != 1 || got["ready"] != 0 || got["off"] != 0 {
		t.Fatalf("deliveries per channel: %v", got)
	}
	// Other teams' apps cannot be selected.
	if _, _, err := f.svc.CreateChannel(ctx, f.team.ID, notify.ChannelRequest{Name: "x", Kind: store.ChannelSlack,
		URL: rcv.URL, Apps: strs("nope")}, ""); err == nil {
		t.Fatal("unknown app accepted")
	}
}

func TestChannelValidation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.svc.Validation.AllowInsecureURLs = false
	cases := []notify.ChannelRequest{
		{Name: "", Kind: store.ChannelSlack, URL: "https://hooks.slack.com/x"},
		{Name: "a", Kind: "sms"},
		{Name: "a", Kind: store.ChannelSlack},
		{Name: "a", Kind: store.ChannelSlack, URL: "http://hooks.slack.com/x"},
		{Name: "a", Kind: store.ChannelWebhook, URL: "https://10.0.0.1/hook"},
		{Name: "a", Kind: store.ChannelWebhook, URL: "https://user:pw@example.com/hook"},
		{Name: "a", Kind: store.ChannelSlack, URL: "https://hooks.slack.com/x", Events: strs("nope")},
		{Name: "a", Kind: store.ChannelSlack, URL: "https://hooks.slack.com/x", Events: strs()},
		{Name: "a", Kind: store.ChannelEmail, Recipients: strs("x@example.com")}, // no SMTP
	}
	for i, c := range cases {
		if _, _, err := f.svc.CreateChannel(ctx, f.team.ID, c, ""); err == nil {
			t.Errorf("case %d accepted: %+v", i, c)
		} else if _, ok := notify.IsInvalid(err); !ok {
			t.Errorf("case %d: %v is not an InvalidError", i, err)
		}
	}
	f.svc.SMTPConfigured = true
	for i, c := range []notify.ChannelRequest{
		{Name: "a", Kind: store.ChannelEmail},
		{Name: "a", Kind: store.ChannelEmail, Recipients: strs("not-an-address")},
		{Name: "a", Kind: store.ChannelEmail, Recipients: strs("x@example.com"), URL: "https://x"},
	} {
		if _, _, err := f.svc.CreateChannel(ctx, f.team.ID, c, ""); err == nil {
			t.Errorf("email case %d accepted", i)
		}
	}
	ch, _ := f.channel(notify.ChannelRequest{Name: "mail", Kind: store.ChannelEmail, Recipients: strs("A@Example.com", "a@example.com", "b@example.com")})
	if len(ch.Recipients) != 2 {
		t.Fatalf("recipients deduplicated case-insensitively: %v", ch.Recipients)
	}
	if _, _, err := f.svc.CreateChannel(ctx, f.team.ID, notify.ChannelRequest{Name: "mail", Kind: store.ChannelEmail, TeamMembers: boolp(true)}, ""); err == nil {
		t.Fatal("duplicate name accepted")
	}
	// Updates keep what is not sent; the kind is fixed.
	up, _, err := f.svc.UpdateChannel(ctx, ch, notify.ChannelRequest{Name: "mail2"})
	if err != nil || up.Name != "mail2" || len(up.Recipients) != 2 {
		t.Fatalf("update: %+v %v", up, err)
	}
	if _, _, err := f.svc.UpdateChannel(ctx, up, notify.ChannelRequest{Kind: store.ChannelSlack}); err == nil {
		t.Fatal("kind change accepted")
	}
}

func boolp(b bool) *bool { return &b }

// TestSecretsEncrypted: with the env keyring, URLs and signing secrets are
// ciphertext bound to the channel; key stats and rotation cover them.
func TestSecretsEncrypted(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	k1, _ := secret.GenerateKey()
	ring, err := secret.NewKeyring("k1:" + k1)
	if err != nil {
		t.Fatal(err)
	}
	f.st.SetEnvKeyring(ring)
	url := "https://hooks.slack.com/services/T000/B000/SECRETPART"
	ch, signing := f.channel(notify.ChannelRequest{Name: "hook", Kind: store.ChannelWebhook, URL: url})
	if !ch.Encrypted || ch.URLHint != "https://hooks.slack.com/…" {
		t.Fatalf("channel: %+v", ch)
	}
	raw, err := sql.Open("postgres", os.Getenv("PAAS_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var stored, keyID string
	raw.QueryRow(`SELECT secrets, key_id FROM notification_channels WHERE id = $1`, ch.ID).Scan(&stored, &keyID)
	if strings.Contains(stored, "SECRETPART") || strings.Contains(stored, signing) || keyID != "k1" || !strings.HasPrefix(stored, "v1:k1:") {
		t.Fatalf("stored in clear: %q (%s)", stored, keyID)
	}
	sec, err := f.st.NotificationChannelSecrets(ctx, ch.ID)
	if err != nil || sec.URL != url || sec.SigningSecret != signing {
		t.Fatalf("decrypt: %+v %v", sec, err)
	}
	// Bound to the channel: another row's ciphertext does not open.
	ch2, _ := f.channel(notify.ChannelRequest{Name: "hook2", Kind: store.ChannelWebhook, URL: url})
	raw.Exec(`UPDATE notification_channels SET secrets = $1 WHERE id = $2`, stored, ch2.ID)
	if _, err := f.st.NotificationChannelSecrets(ctx, ch2.ID); !errors.Is(err, secret.ErrDecrypt) {
		t.Fatalf("swapped ciphertext: %v", err)
	}
	raw.Exec(`DELETE FROM notification_channels WHERE id = $1`, ch2.ID)
	stats, _ := f.st.EnvKeyStats(ctx)
	if stats["k1"] != 1 {
		t.Fatalf("key stats: %v", stats)
	}
	// Rotation re-seals under the new key.
	k2, _ := secret.GenerateKey()
	ring2, _ := secret.NewKeyring("k2:"+k2, "k1:"+k1)
	f.st.SetEnvKeyring(ring2)
	if _, err := f.st.ReencryptEnv(ctx); err != nil {
		t.Fatal(err)
	}
	raw.QueryRow(`SELECT key_id FROM notification_channels WHERE id = $1`, ch.ID).Scan(&keyID)
	if sec, err := f.st.NotificationChannelSecrets(ctx, ch.ID); keyID != "k2" || err != nil || sec.URL != url {
		t.Fatalf("after rotation: %s %v", keyID, err)
	}
	// Without a key the store refuses to start (CheckEnvKeys).
	f.st.SetEnvKeyring(nil)
	if err := f.st.CheckEnvKeys(ctx); err == nil {
		t.Fatal("encrypted channel secrets without a key passed the check")
	}
}

func TestSlackAndDiscordFormats(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	slack, discord := newReceiver(t), newReceiver(t)
	f.channel(notify.ChannelRequest{Name: "s", Kind: store.ChannelSlack, URL: slack.URL})
	f.channel(notify.ChannelRequest{Name: "d", Kind: store.ChannelDiscord, URL: discord.URL})
	e := failedEvent(f, "fmt")
	e.Text = "<!channel> & @everyone"
	f.svc.Emit(ctx, e)
	f.disp.Tick(ctx)

	var s struct {
		Text        string `json:"text"`
		Attachments []struct {
			Color  string           `json:"color"`
			Blocks []map[string]any `json:"blocks"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(slack.bodies[0], &s); err != nil {
		t.Fatal(err)
	}
	if s.Text != "blog: deploy başarısız" || len(s.Attachments) != 1 || s.Attachments[0].Color != "#E01E5A" {
		t.Fatalf("slack: %s", slack.bodies[0])
	}
	// Re-encode without HTML escaping (encoding/json escapes <, > and &).
	var raw any
	json.Unmarshal(slack.bodies[0], &raw)
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.Encode(raw)
	body := sb.String()
	for _, want := range []string{`<https://paas.example.com/deployments/7|blog: deploy başarısız>`, `&lt;!channel&gt; &amp; @everyone`,
		`"type":"button"`, `"url":"https://paas.example.com/deployments/7"`, "web ekibi"} {
		if !strings.Contains(body, want) {
			t.Fatalf("slack payload lacks %q:\n%s", want, body)
		}
	}
	var d struct {
		Embeds []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Color int    `json:"color"`
		} `json:"embeds"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	json.Unmarshal(discord.bodies[0], &d)
	if len(d.Embeds) != 1 || d.Embeds[0].Color != 0xE01E5A || d.Embeds[0].URL != "https://paas.example.com/deployments/7" ||
		d.AllowedMentions.Parse == nil || len(d.AllowedMentions.Parse) != 0 {
		t.Fatalf("discord: %s", discord.bodies[0])
	}
}

// TestPrivateAddressesBlocked: the production client refuses loopback.
func TestPrivateAddressesBlocked(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	rcv := newReceiver(t)
	f.disp.Senders[store.ChannelWebhook] = notify.WebhookSender{Client: notify.NewHTTPClient(time.Second, false)}
	ch, _ := f.channel(notify.ChannelRequest{Name: "h", Kind: store.ChannelWebhook, URL: rcv.URL})
	f.svc.Emit(ctx, failedEvent(f, "p"))
	f.disp.Tick(ctx)
	d := f.deliveries(ch)[0]
	if d.Status != store.DeliveryFailed || !strings.Contains(d.LastError, "özel ya da yerel") || rcv.count() != 0 {
		t.Fatalf("loopback webhook: %+v", d)
	}
	if strings.Contains(d.LastError, "127.0.0.1") {
		t.Fatalf("error leaks the URL: %q", d.LastError)
	}
}

func TestEmailSMTP(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	srv, pool := newFakeSMTP(t, true)
	srv.reject["gone@example.com"] = true
	host, port, _ := strings.Cut(srv.addr, ":")
	var p int
	for _, c := range port {
		p = p*10 + int(c-'0')
	}
	cfg := notify.SMTPConfig{Host: host, Port: p, User: "mailer", Password: "pw", From: "paas@example.com",
		TLSConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}}
	f.disp.Senders[store.ChannelEmail] = notify.EmailSender{Config: cfg}
	f.svc.SMTPConfigured = true

	// bob saved an address and muted deployment.failed; carol wants everything.
	bob, _ := f.st.UpsertUser(ctx, 2, "bob", "", "")
	carol, _ := f.st.UpsertUser(ctx, 3, "carol", "", "")
	f.st.SetMember(ctx, f.team.ID, bob.ID, store.RoleMember)
	f.st.SetMember(ctx, f.team.ID, carol.ID, store.RoleViewer)
	f.st.SetNotificationPrefs(ctx, store.NotificationPrefs{UserID: bob.ID, Email: "bob@example.com", EmailEnabled: true,
		MutedEvents: []string{notify.DeploymentFailed}})
	f.st.SetNotificationPrefs(ctx, store.NotificationPrefs{UserID: carol.ID, Email: "carol@example.com", EmailEnabled: true})
	ch, _ := f.channel(notify.ChannelRequest{Name: "ops mail", Kind: store.ChannelEmail, TeamMembers: boolp(true),
		Recipients: strs("ops@example.com", "gone@example.com", "BOB@example.com")})

	f.svc.Emit(ctx, failedEvent(f, "mail"))
	f.disp.Tick(ctx)
	d := f.deliveries(ch)[0]
	if d.Status != store.DeliverySent || !strings.Contains(d.LastError, "gone@example.com") {
		t.Fatalf("delivery: %+v", d)
	}
	var to []string
	for _, m := range srv.messages() {
		to = append(to, m.To)
	}
	if strings.Join(to, ",") != "carol@example.com,ops@example.com" {
		t.Fatalf("recipients: %v (bob opted out, gone rejected)", to)
	}
	if !srv.tlsUsed || len(srv.auth) != 1 || srv.auth[0] != "\x00mailer\x00pw" {
		t.Fatalf("STARTTLS %v auth %q", srv.tlsUsed, srv.auth)
	}
	msg, err := mail.ReadMessage(strings.NewReader(srv.messages()[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if subject != "[paas] blog: deploy başarısız" || msg.Header.Get("Message-ID") != "<"+d.IdempotencyKey+"@example.com>" {
		t.Fatalf("headers: %q %q", subject, msg.Header.Get("Message-ID"))
	}
	raw, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	body := string(raw)
	for _, want := range []string{"blog: deploy başarısız", "https://paas.example.com/deployments/7", "ops mail", "https://paas.example.com/teams/web/notifications"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %q:\n%s", want, body)
		}
	}

	// A test message ignores opt-outs.
	got, err := f.svc.SendTest(ctx, ch, "alice")
	if err != nil || got.Status != store.DeliverySent {
		t.Fatalf("test: %+v %v", got, err)
	}

	// A server without STARTTLS gets nothing (permanent failure).
	plain, _ := newFakeSMTP(t, false)
	host, port, _ = strings.Cut(plain.addr, ":")
	p = 0
	for _, c := range port {
		p = p*10 + int(c-'0')
	}
	f.disp.Senders[store.ChannelEmail] = notify.EmailSender{Config: notify.SMTPConfig{Host: host, Port: p, From: "paas@example.com"}}
	f.svc.Emit(ctx, failedEvent(f, "mail2"))
	f.disp.Tick(ctx)
	d = f.deliveries(ch)[0]
	if d.Status != store.DeliveryFailed || !strings.Contains(d.LastError, "STARTTLS") || len(plain.messages()) != 0 {
		t.Fatalf("no STARTTLS: %+v", d)
	}
	if err := (notify.SMTPConfig{Host: "x", From: "a@b.c", Security: "none", User: "u"}).Check(); err == nil {
		t.Fatal("clear-text AUTH accepted")
	}
}

// TestWorkerEvents drives the worker: a ready and a failed deployment
// become events through the notifier chain, next to another notifier.
func TestWorkerEvents(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	rcv := newReceiver(t)
	ch, _ := f.channel(notify.ChannelRequest{Name: "all", Kind: store.ChannelSlack, URL: rcv.URL, Events: &notify.EventTypes})
	other := &countingNotifier{}
	w := &worker.Worker{
		Store: f.st, Pipeline: worker.DryRunPipeline{Registry: "r"}, Domain: "paas.test", Log: discard,
		Notifier: notify.Chain(&notify.DeploymentEvents{Emitter: f.svc, Store: f.st}, other, nil),
	}
	f.st.EnqueueDeployment(ctx, f.blog.ID, strings.Repeat("a", 40), "main", "first\nbody")
	if ok, err := w.ProcessOne(ctx); !ok || err != nil {
		t.Fatalf("process: %v %v", ok, err)
	}
	w.Pipeline = failing{}
	f.st.EnqueueDeployment(ctx, f.blog.ID, strings.Repeat("b", 40), "main", "second")
	w.ProcessOne(ctx)
	ds := f.deliveries(ch)
	if len(ds) != 2 || ds[1].EventType != notify.DeploymentReady || ds[0].EventType != notify.DeploymentFailed {
		t.Fatalf("events: %+v", ds)
	}
	var e notify.Event
	json.Unmarshal(ds[0].Payload, &e)
	if e.App != "blog" || e.Team != "web" || !strings.Contains(e.URL, "/deployments/") || e.Severity != notify.SeverityError {
		t.Fatalf("failed event: %+v", e)
	}
	if other.finished != 2 {
		t.Fatalf("chained notifier saw %d", other.finished)
	}

	// Rollout outcomes: an automatic rollback, and a finished canary
	// (production changed). A guarded promotion is not an event.
	var readyEv notify.Event
	json.Unmarshal(ds[1].Payload, &readyEv)
	dep, err := f.st.GetDeployment(ctx, readyEv.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	from := dep.ID
	n := &notify.DeploymentEvents{Emitter: f.svc, Store: f.st}
	ro := store.Rollout{ID: 9, AppID: f.blog.ID, AppName: "blog", ToDeploymentID: dep.ID, FromDeploymentID: &from,
		Mode: store.RolloutCanary, State: store.RolloutRolledBack, Reason: "5xx %12"}
	n.RolloutFinished(ctx, dep, ro)
	n.RolloutFinished(ctx, dep, ro) // repeated by another replica
	ro.ID, ro.State = 10, store.RolloutPromoted
	n.RolloutFinished(ctx, dep, ro)
	ro.ID, ro.Mode = 11, store.RolloutGuarded
	n.RolloutFinished(ctx, dep, ro)
	ds = f.deliveries(ch)
	if len(ds) != 4 || ds[0].EventType != notify.ProductionChanged || ds[1].EventType != notify.RolloutRolledBack {
		t.Fatalf("rollout events: %d %+v", len(ds), ds)
	}
}

type countingNotifier struct{ finished int }

func (c *countingNotifier) DeploymentStarted(context.Context, store.Deployment) error { return nil }
func (c *countingNotifier) DeploymentFinished(context.Context, store.Deployment, worker.Result) error {
	c.finished++
	return nil
}

type failing struct{}

func (failing) Build(context.Context, store.Deployment, store.BuildSettings, worker.Logger) (worker.BuildResult, error) {
	return worker.BuildResult{}, errors.New("npm ERR! missing script: build")
}
func (failing) Deploy(context.Context, store.Deployment, string, worker.Logger) error { return nil }
