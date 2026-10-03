package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

type sseEvent struct {
	id, event, data string
}

// readEvents parses an SSE body into a channel, closed at EOF.
func readEvents(body io.Reader) <-chan sseEvent {
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(body)
		var ev sseEvent
		var data []string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if data != nil || ev.event != "" {
					ev.data = strings.Join(data, "\n")
					out <- ev
				}
				ev, data = sseEvent{}, nil
			case strings.HasPrefix(line, ":"), strings.HasPrefix(line, "retry:"):
			case strings.HasPrefix(line, "id: "):
				ev.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				ev.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				data = append(data, line[6:])
			}
		}
	}()
	return out
}

func next(t *testing.T, c <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-c:
		if !ok {
			t.Fatal("stream closed early")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return sseEvent{}
}

type streamEnv struct {
	st  *store.Store
	srv *httptest.Server
}

func setupStream(t *testing.T, events api.Notifier, rt api.RuntimeLogs) *streamEnv {
	t.Helper()
	st := testdb.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer((&api.Server{
		Store: st, Domain: domain, APIToken: token, WebhookSecret: secret, Log: log,
		Sessions: auth.New(token), Events: events, RuntimeLogs: rt,
		Stream: api.StreamTiming{Poll: 50 * time.Millisecond, FinishGrace: 200 * time.Millisecond},
	}).Handler())
	t.Cleanup(srv.Close)
	return &streamEnv{st: st, srv: srv}
}

func (e *streamEnv) open(t *testing.T, path string, hdr map[string]string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *streamEnv) deployment(t *testing.T) store.Deployment {
	t.Helper()
	ctx := context.Background()
	app, err := e.st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := e.st.EnqueueDeployment(ctx, app.ID, strings.Repeat("a", 40), "main", "first")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func startHub(t *testing.T) *store.Hub {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hub := store.NewHub(os.Getenv("PAAS_TEST_DATABASE_URL"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	go hub.Run(ctx)
	select {
	case <-hub.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("hub did not start")
	}
	return hub
}

// The stream replays the backlog, follows new lines through LISTEN/NOTIFY,
// reports status changes and closes after the final status.
func TestStreamLogsLive(t *testing.T) {
	if os.Getenv("PAAS_TEST_DATABASE_URL") == "" {
		t.Skip("PAAS_TEST_DATABASE_URL not set")
	}
	e := setupStream(t, nil, nil)
	hub := startHub(t)
	e.srv.Config.Handler = (&api.Server{
		Store: e.st, Domain: domain, APIToken: token, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Events: hub,
		// No heartbeat re-reads during the test: only NOTIFY can deliver.
		Stream: api.StreamTiming{Heartbeat: time.Hour, FinishGrace: 200 * time.Millisecond},
	}).Handler()

	ctx := context.Background()
	d := e.deployment(t)
	e.st.AppendLog(ctx, d.ID, "line one")
	e.st.AppendLog(ctx, d.ID, "line two")
	if _, err := e.st.ClaimNext(ctx); err != nil {
		t.Fatal(err)
	}

	resp := e.open(t, fmt.Sprintf("/api/deployments/%d/logs/stream", d.ID), nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := readEvents(resp.Body)

	first := next(t, events)
	if first.data != "line one" || first.id == "" {
		t.Fatalf("first event = %+v", first)
	}
	if ev := next(t, events); ev.data != "line two" {
		t.Fatalf("second event = %+v", ev)
	}
	if ev := next(t, events); ev.event != "status" || !strings.Contains(ev.data, `"building"`) ||
		!strings.Contains(ev.data, `"final":false`) {
		t.Fatalf("status event = %+v", ev)
	}

	e.st.AppendLog(ctx, d.ID, "live\nmulti-line")
	if ev := next(t, events); ev.data != "live\nmulti-line" {
		t.Fatalf("live event = %+v", ev)
	}
	e.st.SetStatus(ctx, d.ID, store.StatusDeploying)
	if ev := next(t, events); ev.event != "status" || !strings.Contains(ev.data, `"deploying"`) {
		t.Fatalf("status event = %+v", ev)
	}

	e.st.MarkFailed(ctx, d.ID, "boom")
	e.st.AppendLog(ctx, d.ID, "ERROR: boom") // written right after the status, still delivered
	if ev := next(t, events); ev.data != "ERROR: boom" {
		t.Fatalf("late line = %+v", ev)
	}
	var st struct {
		Status, Error string
		Final         bool
	}
	ev := next(t, events)
	if ev.event != "status" || json.Unmarshal([]byte(ev.data), &st) != nil ||
		st.Status != "failed" || st.Error != "boom" || !st.Final {
		t.Fatalf("final event = %+v", ev)
	}
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("event after the final status")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream not closed after the final status")
	}

	// Resuming with Last-Event-ID replays only what came after it.
	resp = e.open(t, fmt.Sprintf("/api/deployments/%d/logs/stream", d.ID), map[string]string{"Last-Event-ID": first.id})
	events = readEvents(resp.Body)
	if ev := next(t, events); ev.data != "line two" {
		t.Fatalf("resumed first event = %+v", ev)
	}
}

// Without a Notifier the stream polls; a session cookie authorizes it,
// because EventSource cannot send an Authorization header.
func TestStreamLogsPollingAndAuth(t *testing.T) {
	e := setupStream(t, nil, nil)
	ctx := context.Background()
	d := e.deployment(t)
	path := fmt.Sprintf("/api/deployments/%d/logs/stream?after=0", d.ID)

	if resp := e.open(t, path, map[string]string{"Authorization": ""}); resp.StatusCode != 401 {
		t.Fatalf("no auth: status %d", resp.StatusCode)
	}

	rec := httptest.NewRecorder()
	auth.New(token).Issue(rec, httptest.NewRequest("POST", "/login", nil))
	cookie := rec.Result().Cookies()[0]
	resp := e.open(t, path, map[string]string{"Authorization": "", "Cookie": cookie.Name + "=" + cookie.Value})
	if resp.StatusCode != 200 {
		t.Fatalf("cookie auth: status %d", resp.StatusCode)
	}
	events := readEvents(resp.Body)
	if ev := next(t, events); ev.event != "status" || !strings.Contains(ev.data, "queued") {
		t.Fatalf("first event = %+v", ev)
	}
	e.st.AppendLog(ctx, d.ID, "polled")
	if ev := next(t, events); ev.data != "polled" {
		t.Fatalf("event = %+v", ev)
	}

	// Unsafe methods never accept the cookie (they would need CSRF checks).
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/apps/blog/rollback", strings.NewReader(`{"deployment_id":1}`))
	req.Header.Set("Cookie", cookie.Name+"="+cookie.Value)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 401 {
		t.Fatalf("cookie POST: status %d, want 401", r2.StatusCode)
	}

	if resp := e.open(t, "/api/deployments/999/logs/stream", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown deployment: status %d", resp.StatusCode)
	}
}

type fakeRuntime struct {
	err   error
	got   string
	lines string
}

func (f *fakeRuntime) RuntimeLogs(_ context.Context, app, sha string, follow bool, tail int64, w io.Writer) error {
	f.got = fmt.Sprintf("%s %s %v %d", app, sha[:7], follow, tail)
	if f.err != nil {
		return f.err
	}
	io.WriteString(w, f.lines)
	return nil
}

type notFound struct{}

func (notFound) Error() string  { return "no pods for deployment app-blog/d-aaaaaaa" }
func (notFound) NotFound() bool { return true }

func TestRuntimeLogs(t *testing.T) {
	rt := &fakeRuntime{lines: "listening on :8080\nGET / 200\npartial"}
	e := setupStream(t, nil, rt)
	d := e.deployment(t)
	path := fmt.Sprintf("/api/apps/blog/deployments/%d/runtime-logs", d.ID)

	resp := e.open(t, path+"?follow=1&tail=50", nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != rt.lines || rt.got != "blog aaaaaaa true 50" {
		t.Fatalf("plain: %d %q (called with %q)", resp.StatusCode, body, rt.got)
	}

	resp = e.open(t, path, map[string]string{"Accept": "text/event-stream"})
	events := readEvents(resp.Body)
	for _, want := range []string{"listening on :8080", "GET / 200", "partial"} {
		if ev := next(t, events); ev.data != want {
			t.Fatalf("event = %+v, want %q", ev, want)
		}
	}
	if ev := next(t, events); ev.event != "end" {
		t.Fatalf("last event = %+v", ev)
	}
	if rt.got != "blog aaaaaaa false 200" {
		t.Fatalf("defaults: called with %q", rt.got)
	}

	rt.err = notFound{}
	if resp := e.open(t, path, nil); resp.StatusCode != 404 {
		t.Fatalf("no pods: status %d", resp.StatusCode)
	}
	if resp := e.open(t, path+"?tail=0", nil); resp.StatusCode != 400 {
		t.Fatalf("tail=0: status %d", resp.StatusCode)
	}
	if resp := e.open(t, "/api/apps/nope/deployments/1/runtime-logs", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown app: status %d", resp.StatusCode)
	}

	// Dry-run deployer: no runtime logs.
	e2 := setupStream(t, nil, nil)
	if resp := e2.open(t, "/api/apps/blog/deployments/1/runtime-logs", nil); resp.StatusCode != 501 {
		t.Fatalf("without deployer: status %d", resp.StatusCode)
	}
}
