package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

// Notifier wakes a subscriber when a deployment gets a log line or a new
// status (store.Hub, fed by Postgres LISTEN/NOTIFY).
type Notifier interface {
	Subscribe(deploymentID int64) (<-chan struct{}, func())
}

// RuntimeLogs writes the container logs of a deployment's pod to w
// (deploy.Kubernetes). With follow it returns when ctx ends or the pod stops.
type RuntimeLogs interface {
	RuntimeLogs(ctx context.Context, app, sha string, follow bool, tail int64, w io.Writer) error
}

// StreamTiming tunes the log streams; zero fields use the defaults.
type StreamTiming struct {
	// Heartbeat keeps idle streams alive through proxies (15s).
	Heartbeat time.Duration
	// Poll re-reads the database when there is no Notifier (1s).
	Poll time.Duration
	// FinishGrace is how long a stream waits after a deployment finished for
	// its last lines (2s): the worker logs the alias URLs after marking it ready.
	FinishGrace time.Duration
}

func (t StreamTiming) withDefaults() StreamTiming {
	if t.Heartbeat <= 0 {
		t.Heartbeat = 15 * time.Second
	}
	if t.Poll <= 0 {
		t.Poll = time.Second
	}
	if t.FinishGrace <= 0 {
		t.FinishGrace = 2 * time.Second
	}
	return t
}

const streamBatch = 500

// oldFinish: a deployment finished longer ago than this needs no grace for
// late log lines, even allowing for clock skew between us and the database.
const oldFinish = 30 * time.Second

// Unwrap lets http.ResponseController reach the real writer's Flush.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// sseWriter writes Server-Sent Events and flushes after each one. It is
// safe for concurrent use (log writer and heartbeat goroutine).
type sseWriter struct {
	mu  sync.Mutex
	w   io.Writer
	rc  *http.ResponseController
	err error
}

func newSSE(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx-style proxies must not buffer
	w.WriteHeader(http.StatusOK)
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

// send writes one event; id and event may be empty. Multi-line data is
// split into several data fields, which the client joins with "\n".
func (s *sseWriter) send(id, event, data string) error {
	var b bytes.Buffer
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	if event != "" {
		b.WriteString("event: " + event + "\n")
	}
	for _, l := range strings.Split(strings.ReplaceAll(data, "\r", ""), "\n") {
		b.WriteString("data: " + l + "\n")
	}
	b.WriteString("\n")
	return s.raw(b.Bytes())
}

func (s *sseWriter) comment(c string) error { return s.raw([]byte(": " + c + "\n\n")) }

func (s *sseWriter) raw(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, s.err = s.w.Write(p); s.err == nil {
		s.err = s.rc.Flush()
	}
	return s.err
}

type statusEvent struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// Final is set on the last event before the server closes the stream.
	Final bool `json:"final"`
}

func (s *sseWriter) status(d store.Deployment, final bool) error {
	b, _ := json.Marshal(statusEvent{Status: d.Status, Error: d.Error, Final: final})
	return s.send("", "status", string(b))
}

// streamLogs serves a deployment's build log as Server-Sent Events: first
// the lines after Last-Event-ID (or ?after=), then new lines as they are
// written. Each line's event id is its row id, so a reconnecting
// EventSource resumes where it stopped. "status" events report status
// changes; the one with "final": true is sent when the deployment is ready
// or failed, and the server then closes the stream.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var after int64
	for _, v := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")} {
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "after / Last-Event-ID must be a non-negative integer")
			return
		}
		after = n
		break
	}
	ctx := r.Context()
	timing := s.Stream.withDefaults()
	if _, err := s.Store.GetDeployment(ctx, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}

	// Subscribe before the first query, so nothing falls in between.
	var wake <-chan struct{}
	poll := time.NewTicker(timing.Poll)
	defer poll.Stop()
	if s.Events != nil {
		c, cancel := s.Events.Subscribe(id)
		defer cancel()
		wake = c
		poll.Stop() // the heartbeat still re-reads as a safety net
	}
	heartbeat := time.NewTicker(timing.Heartbeat)
	defer heartbeat.Stop()

	sse := newSSE(w)
	if err := sse.raw([]byte("retry: 3000\n\n")); err != nil {
		return
	}
	var (
		lastStatus string
		finishAt   time.Time
	)
	for {
		for {
			lines, err := s.Store.Logs(ctx, id, after, streamBatch)
			if err != nil {
				if ctx.Err() == nil {
					s.Log.Error("stream logs", "deployment", id, "err", err)
				}
				return
			}
			for _, l := range lines {
				if sse.send(strconv.FormatInt(l.ID, 10), "", l.Line) != nil {
					return
				}
				after = l.ID
			}
			if len(lines) < streamBatch {
				break
			}
		}

		d, err := s.Store.GetDeployment(ctx, id)
		if err != nil {
			if ctx.Err() == nil {
				s.Log.Error("stream logs", "deployment", id, "err", err)
			}
			return
		}
		if d.Finished() {
			if finishAt.IsZero() {
				// The grace period starts when this stream sees the end, not
				// at finished_at: that timestamp comes from the database
				// clock, which may be skewed against ours by more than the
				// grace itself. Only long-finished deployments close at once.
				wait := timing.FinishGrace
				if d.FinishedAt != nil && time.Since(*d.FinishedAt) > oldFinish {
					wait = 0
				}
				finishAt = time.Now().Add(wait)
			}
			if !time.Now().Before(finishAt) {
				sse.status(d, true)
				return
			}
		} else if d.Status != lastStatus {
			if sse.status(d, false) != nil {
				return
			}
		}
		lastStatus = d.Status

		var grace <-chan time.Time
		if !finishAt.IsZero() {
			grace = time.After(time.Until(finishAt))
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-poll.C:
		case <-grace:
		case <-heartbeat.C:
			if sse.comment("ping") != nil {
				return
			}
		}
	}
}

// runtimeLogs streams the pod logs of a deployment: ?tail=N lines (default
// 200), ?follow=1 keeps the stream open. Clients sending
// "Accept: text/event-stream" (EventSource) get SSE with one event per line
// and a final "end" event; everyone else gets chunked text/plain.
func (s *Server) runtimeLogs(w http.ResponseWriter, r *http.Request) {
	if s.RuntimeLogs == nil {
		writeError(w, http.StatusNotImplemented, "runtime logs need PAAS_DEPLOYER=kubernetes")
		return
	}
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	tail := int64(200)
	if v := q.Get("tail"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > 10000 {
			writeError(w, http.StatusBadRequest, "tail must be between 1 and 10000")
			return
		}
		tail = n
	}
	follow := q.Get("follow") == "1" || q.Get("follow") == "true"
	d, err := s.Store.GetDeployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.AppID != app.ID) {
		writeError(w, http.StatusNotFound, "deployment not found for this app")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	// Headers are sent on the first byte, so an early error (no pod yet)
	// can still become a proper status code.
	out := &lazyStream{w: w, sse: strings.Contains(r.Header.Get("Accept"), "text/event-stream")}
	ctx, cancel := context.WithCancel(r.Context())
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		out.heartbeat(ctx, s.Stream.withDefaults().Heartbeat)
	}()
	err = s.RuntimeLogs.RuntimeLogs(ctx, app.Name, d.CommitSHA, follow, tail, out)
	cancel()
	<-hbDone // no writes after the handler returns
	if r.Context().Err() != nil {
		return // client went away
	}
	if err != nil {
		if !out.started() {
			var nf interface{ NotFound() bool }
			if errors.As(err, &nf) && nf.NotFound() {
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				s.Log.Error("runtime logs", "app", app.Name, "deployment", id, "err", err)
				writeError(w, http.StatusBadGateway, err.Error())
			}
			return
		}
		out.finish("error: " + err.Error())
		return
	}
	out.finish("")
}

// lazyStream adapts a raw log byte stream to SSE events or flushed
// text/plain chunks, writing the response headers on first use.
type lazyStream struct {
	mu   sync.Mutex
	w    http.ResponseWriter
	sse  bool
	ev   *sseWriter // SSE mode
	rc   *http.ResponseController
	part []byte // incomplete last line (SSE mode)
	err  error
}

func (l *lazyStream) started() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ev != nil || l.rc != nil
}

func (l *lazyStream) start() {
	if l.ev != nil || l.rc != nil {
		return
	}
	if l.sse {
		l.ev = newSSE(l.w)
		return
	}
	l.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	l.w.Header().Set("X-Content-Type-Options", "nosniff")
	l.w.WriteHeader(http.StatusOK)
	l.rc = http.NewResponseController(l.w)
}

func (l *lazyStream) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return 0, l.err
	}
	l.start()
	if !l.sse {
		if _, l.err = l.w.Write(p); l.err == nil {
			l.err = l.rc.Flush()
		}
		return len(p), l.err
	}
	l.part = append(l.part, p...)
	for {
		i := bytes.IndexByte(l.part, '\n')
		if i < 0 {
			break
		}
		if l.err = l.ev.send("", "", string(l.part[:i])); l.err != nil {
			return 0, l.err
		}
		l.part = l.part[i+1:]
	}
	return len(p), nil
}

// finish flushes a trailing partial line and, for SSE, sends "end" so the
// browser does not reconnect and replay the tail.
func (l *lazyStream) finish(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.start()
	if !l.sse {
		if msg != "" {
			fmt.Fprintf(l.w, "\n[paas] %s\n", msg)
		}
		return
	}
	if len(l.part) > 0 {
		l.ev.send("", "", string(l.part))
		l.part = nil
	}
	l.ev.send("", "end", msg)
}

func (l *lazyStream) heartbeat(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.mu.Lock()
			if l.sse && l.ev != nil {
				l.ev.comment("ping")
			}
			l.mu.Unlock()
		}
	}
}
