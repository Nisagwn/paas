package store

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Channels raised by the triggers in migrations/003_log_notify.sql.
const (
	ChannelLogs   = "deployment_logs"
	ChannelStatus = "deployment_status"
)

// Hub turns Postgres notifications into per-deployment wake-ups. It holds one
// dedicated LISTEN connection for the whole process and fans out to any
// number of subscribers.
//
// A wake-up carries no data: subscribers re-query rows newer than what they
// have seen, so a coalesced or lost signal never loses a line. After the
// listen connection is re-established every subscriber is woken, because
// notifications sent while it was down are gone.
type Hub struct {
	url string
	log *slog.Logger

	mu    sync.Mutex
	subs  map[int64]map[chan struct{}]struct{}
	ready chan struct{}
}

func NewHub(url string, log *slog.Logger) *Hub {
	return &Hub{url: url, log: log, subs: map[int64]map[chan struct{}]struct{}{}, ready: make(chan struct{})}
}

// Subscribe returns a channel that receives a value whenever a log line is
// added to deployment id or its status changes. Signals are coalesced (the
// channel has a buffer of one). Call cancel when done.
func (h *Hub) Subscribe(id int64) (<-chan struct{}, func()) {
	c := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[id] == nil {
		h.subs[id] = map[chan struct{}]struct{}{}
	}
	h.subs[id][c] = struct{}{}
	h.mu.Unlock()
	return c, func() {
		h.mu.Lock()
		delete(h.subs[id], c)
		if len(h.subs[id]) == 0 {
			delete(h.subs, id)
		}
		h.mu.Unlock()
	}
}

// Ready is closed once the hub listens on its channels for the first time.
func (h *Hub) Ready() <-chan struct{} { return h.ready }

func (h *Hub) wake(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs[id] {
		signal(c)
	}
}

func (h *Hub) wakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cs := range h.subs {
		for c := range cs {
			signal(c)
		}
	}
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// Run listens until ctx is cancelled. pq.Listener reconnects on its own
// with backoff; Run only returns early if LISTEN itself is rejected.
func (h *Hub) Run(ctx context.Context) error {
	l := pq.NewListener(h.url, time.Second, 30*time.Second, func(ev pq.ListenerEventType, err error) {
		switch ev {
		case pq.ListenerEventDisconnected:
			h.log.Warn("log listener disconnected", "err", err)
		case pq.ListenerEventReconnected:
			h.log.Info("log listener reconnected")
		case pq.ListenerEventConnectionAttemptFailed:
			h.log.Warn("log listener connect failed", "err", err)
		}
	})
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	defer l.Close()

	// Listen blocks until the connection is up; Close (on ctx) unblocks it.
	for _, ch := range []string{ChannelLogs, ChannelStatus} {
		if err := l.Listen(ch); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	close(h.ready)
	h.wakeAll() // anything that happened before LISTEN took effect

	ping := time.NewTicker(90 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case n, ok := <-l.Notify:
			if !ok {
				return nil
			}
			if n == nil { // reconnected: notifications may have been missed
				h.wakeAll()
				continue
			}
			if id, err := strconv.ParseInt(n.Extra, 10, 64); err == nil {
				h.wake(id)
			}
		case <-ping.C:
			// Detects a dead connection that TCP has not noticed yet.
			go l.Ping()
		}
	}
}
