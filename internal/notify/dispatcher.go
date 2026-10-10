package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/lib/pq"

	"github.com/nisagwn/paas/internal/store"
)

// Dispatcher defaults.
const (
	DefaultInterval    = 5 * time.Second
	DefaultBatch       = 20
	DefaultMaxAttempts = 8
	DefaultRateLimit   = 30 // per channel and minute
	DefaultLease       = 2 * time.Minute
	DefaultRetention   = 30 * 24 * time.Hour
	// Backoff after the n-th failed attempt: BaseBackoff·2^(n-1), at most MaxBackoff.
	DefaultBaseBackoff = 30 * time.Second
	DefaultMaxBackoff  = time.Hour
	// OutboxChannel is the LISTEN channel the outbox trigger notifies.
	OutboxChannel = "notification_outbox"
)

// Dispatcher sends the outbox. Several dispatchers (control plane
// replicas) share it: deliveries are leased with SKIP LOCKED, and a lease
// that runs out (crash mid-send) is claimed again, so every delivery is
// attempted at least once; receivers deduplicate with the idempotency key.
type Dispatcher struct {
	Store   *store.Store
	Senders map[string]Sender
	Log     *slog.Logger

	Interval    time.Duration
	Batch       int
	Parallel    int
	MaxAttempts int
	// RateLimit is the most deliveries a channel sends per RateWindow (a
	// minute); the rest wait. Test messages are not limited.
	RateLimit   int
	RateWindow  time.Duration
	Lease       time.Duration
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Retention   time.Duration
	// ListenURL, when set, wakes the dispatcher through LISTEN as soon as
	// any replica queues a delivery.
	ListenURL string

	// Now is the clock (tests); nil is time.Now. Jitter scales a backoff
	// (tests make it 1); nil adds up to ±10 %.
	Now    func() time.Time
	Jitter func() float64

	once   sync.Once
	kicks  chan struct{}
	pruned time.Time
}

func (d *Dispatcher) defaults() {
	d.once.Do(func() {
		if d.Interval <= 0 {
			d.Interval = DefaultInterval
		}
		if d.Batch <= 0 {
			d.Batch = DefaultBatch
		}
		if d.Parallel <= 0 {
			d.Parallel = 4
		}
		if d.MaxAttempts <= 0 {
			d.MaxAttempts = DefaultMaxAttempts
		}
		if d.RateWindow <= 0 {
			d.RateWindow = time.Minute
		}
		if d.Lease <= 0 {
			d.Lease = DefaultLease
		}
		if d.BaseBackoff <= 0 {
			d.BaseBackoff = DefaultBaseBackoff
		}
		if d.MaxBackoff <= 0 {
			d.MaxBackoff = DefaultMaxBackoff
		}
		if d.Retention <= 0 {
			d.Retention = DefaultRetention
		}
		if d.Log == nil {
			d.Log = slog.New(slog.DiscardHandler)
		}
		if d.Now == nil {
			d.Now = time.Now
		}
		if d.Jitter == nil {
			d.Jitter = func() float64 { return 0.9 + rand.Float64()*0.2 }
		}
		d.kicks = make(chan struct{}, 1)
	})
}

// Kick asks Run to look at the outbox now. It never blocks.
func (d *Dispatcher) Kick() {
	d.defaults()
	select {
	case d.kicks <- struct{}{}:
	default:
	}
}

// Run sends due deliveries every Interval, when kicked and, with
// ListenURL, when any replica queues one, until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	d.defaults()
	if d.ListenURL != "" {
		go d.listen(ctx)
	}
	t := time.NewTicker(d.Interval)
	defer t.Stop()
	for {
		for ctx.Err() == nil && d.Tick(ctx) == d.Batch {
			// A full batch: more may be due.
		}
		if now := d.Now(); now.Sub(d.pruned) > time.Hour {
			d.pruned = now
			if n, err := d.Store.PruneNotifications(ctx, now.Add(-d.Retention)); err != nil {
				d.Log.Error("notifications: prune", "err", err)
			} else if n > 0 {
				d.Log.Info("notifications: pruned old events", "events", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.kicks:
		}
	}
}

// listen turns outbox notifications into kicks.
func (d *Dispatcher) listen(ctx context.Context) {
	l := pq.NewListener(d.ListenURL, time.Second, 30*time.Second, func(ev pq.ListenerEventType, err error) {
		if ev == pq.ListenerEventConnectionAttemptFailed {
			d.Log.Warn("notifications: listener connect failed", "err", err)
		}
	})
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	defer l.Close()
	if err := l.Listen(OutboxChannel); err != nil {
		if ctx.Err() == nil {
			d.Log.Warn("notifications: LISTEN failed; polling only", "err", err)
		}
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-l.Notify:
			if !ok {
				return
			}
			d.Kick()
		case <-time.After(90 * time.Second):
			go l.Ping()
		}
	}
}

// Tick claims and sends one batch of due deliveries and returns how many
// it claimed.
func (d *Dispatcher) Tick(ctx context.Context) int {
	d.defaults()
	jobs, err := d.Store.ClaimNotificationDeliveries(ctx, d.Now(), d.Batch, d.Lease, 0)
	if err != nil {
		if ctx.Err() == nil {
			d.Log.Error("notifications: claim", "err", err)
		}
		return 0
	}
	sem := make(chan struct{}, d.Parallel)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j store.NotificationJob) {
			defer wg.Done()
			defer func() { <-sem }()
			d.process(ctx, j)
		}(j)
	}
	wg.Wait()
	return len(jobs)
}

// DeliverNow sends delivery id at once if it is due (test messages).
func (d *Dispatcher) DeliverNow(ctx context.Context, id int64) {
	d.defaults()
	jobs, err := d.Store.ClaimNotificationDeliveries(ctx, d.Now(), 1, d.Lease, id)
	if err != nil {
		d.Log.Error("notifications: claim", "delivery", id, "err", err)
		return
	}
	for _, j := range jobs {
		d.process(ctx, j)
	}
}

// process makes one attempt of a claimed delivery and records it.
func (d *Dispatcher) process(ctx context.Context, j store.NotificationJob) {
	defer func() {
		if p := recover(); p != nil {
			d.Log.Error("notifications: sender panicked", "delivery", j.DeliveryID, "panic", p)
			d.finish(ctx, j, store.DeliveryOutcome{Status: store.DeliveryFailed, Error: "gönderici beklenmedik biçimde durdu"})
		}
	}()
	o := d.attempt(ctx, j)
	d.finish(ctx, j, o)
	log := d.Log.With("delivery", j.DeliveryID, "channel", j.Channel.ID, "kind", j.Channel.Kind, "type", j.EventType, "attempt", j.Attempts)
	switch o.Status {
	case store.DeliverySent:
		log.Info("notification sent", "code", o.Code)
	case store.DeliveryPending:
		if !o.Refund {
			log.Warn("notification failed, will retry", "err", o.Error, "next", o.NextAttempt)
		}
	default:
		log.Warn("notification not delivered", "status", o.Status, "err", o.Error)
	}
}

func (d *Dispatcher) finish(ctx context.Context, j store.NotificationJob, o store.DeliveryOutcome) {
	// The outcome is recorded even if ctx ended during the send.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := d.Store.FinishNotificationDelivery(fctx, j.DeliveryID, d.Now(), o); err != nil {
		d.Log.Error("notifications: record outcome", "delivery", j.DeliveryID, "err", err)
	}
}

func (d *Dispatcher) attempt(ctx context.Context, j store.NotificationJob) store.DeliveryOutcome {
	now := d.Now()
	if j.EventType != Test && !j.Channel.Enabled {
		return store.DeliveryOutcome{Status: store.DeliverySkipped, Error: "kanal kapalı"}
	}
	if d.RateLimit > 0 && j.EventType != Test {
		n, err := d.Store.NotificationsSentSince(ctx, j.Channel.ID, now.Add(-d.RateWindow))
		if err != nil {
			return d.retry(j, now, &SendError{Msg: "veritabanı: " + err.Error()})
		}
		if n >= d.RateLimit {
			// Not an attempt: wait for the window to free up.
			wait := d.RateWindow / time.Duration(d.RateLimit)
			return store.DeliveryOutcome{Status: store.DeliveryPending, Error: "hız sınırı: kanal dakikada en fazla " +
				strconv.Itoa(d.RateLimit) + " mesaj gönderir", NextAttempt: now.Add(max(wait, time.Second)), Refund: true}
		}
	}
	var e Event
	if err := json.Unmarshal(j.Payload, &e); err != nil {
		return store.DeliveryOutcome{Status: store.DeliveryFailed, Error: "olay okunamadı: " + err.Error()}
	}
	sender := d.Senders[j.Channel.Kind]
	if sender == nil {
		return store.DeliveryOutcome{Status: store.DeliveryFailed, Error: "bu kanal türü için gönderici yok: " + j.Channel.Kind}
	}
	m := Message{Event: e, Channel: j.Channel, IdempotencyKey: j.IdempotencyKey, Attempt: j.Attempts}
	if j.Channel.Kind == store.ChannelEmail {
		rcpts, err := d.Store.NotificationEmailRecipients(ctx, j.Channel, j.EventType)
		if err != nil {
			return d.retry(j, now, &SendError{Msg: "alıcılar okunamadı: " + err.Error()})
		}
		if len(rcpts) == 0 {
			return store.DeliveryOutcome{Status: store.DeliverySkipped, Error: "alıcı yok (adresler tercihlerde kapatılmış ya da tanımsız)"}
		}
		m.Recipients = rcpts
	} else {
		sec, err := d.Store.NotificationChannelSecrets(ctx, j.Channel.ID)
		if err != nil {
			return store.DeliveryOutcome{Status: store.DeliveryFailed, Error: "kanal sırları çözülemedi: " + err.Error()}
		}
		if sec.URL == "" {
			return store.DeliveryOutcome{Status: store.DeliveryFailed, Error: "kanalın URL'si yok"}
		}
		m.Secrets = sec
	}
	res, err := sender.Send(ctx, m)
	if err == nil {
		return store.DeliveryOutcome{Status: store.DeliverySent, Code: res.Code, Error: res.Note}
	}
	var se *SendError
	if !errors.As(err, &se) {
		se = &SendError{Msg: err.Error()}
	}
	if se.Permanent {
		return store.DeliveryOutcome{Status: store.DeliveryFailed, Code: se.Code, Error: se.Error()}
	}
	return d.retry(j, now, se)
}

// retry schedules the next attempt, or gives up after MaxAttempts.
func (d *Dispatcher) retry(j store.NotificationJob, now time.Time, se *SendError) store.DeliveryOutcome {
	o := store.DeliveryOutcome{Status: store.DeliveryPending, Code: se.Code, Error: se.Error()}
	if j.Attempts >= d.MaxAttempts {
		o.Status = store.DeliveryFailed
		o.Error = se.Error() + " (" + strconv.Itoa(j.Attempts) + " denemeden sonra vazgeçildi)"
		return o
	}
	o.NextAttempt = now.Add(max(d.Backoff(j.Attempts), se.RetryAfter))
	return o
}

// Backoff is the wait after the n-th failed attempt.
func (d *Dispatcher) Backoff(n int) time.Duration {
	d.defaults()
	b := d.BaseBackoff
	for i := 1; i < n && b < d.MaxBackoff; i++ {
		b *= 2
	}
	b = min(b, d.MaxBackoff)
	return time.Duration(float64(b) * d.Jitter())
}
