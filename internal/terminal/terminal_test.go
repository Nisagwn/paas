package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/wsock"
)

// ---- fakes ----

type exitErr int

func (e exitErr) Error() string   { return fmt.Sprintf("command terminated with exit code %d", int(e)) }
func (e exitErr) ExitStatus() int { return int(e) }

// fakeCluster echoes stdin as "<input>" and exits when it reads "exit N".
type fakeCluster struct {
	noShell bool
	failure error

	mu       sync.Mutex
	reqs     []ExecRequest
	sizes    []Size
	canceled bool
}

func (f *fakeCluster) Pods(context.Context, store.Deployment) ([]Pod, error) { return nil, nil }

func (f *fakeCluster) Exec(ctx context.Context, req ExecRequest) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.noShell {
		return fmt.Errorf(`OCI runtime exec failed: exec: "/bin/sh": stat /bin/sh: no such file or directory: %w`, ErrNoShell)
	}
	if f.failure != nil {
		return f.failure
	}
	go func() {
		for {
			s, ok := req.Resize.Next()
			if !ok {
				return
			}
			f.mu.Lock()
			f.sizes = append(f.sizes, s)
			f.mu.Unlock()
		}
	}()
	lines := make(chan string)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := req.Stdin.Read(buf)
			if err != nil {
				close(lines)
				return
			}
			lines <- string(buf[:n])
		}
	}()
	for {
		select {
		case <-ctx.Done():
			f.mu.Lock()
			f.canceled = true
			f.mu.Unlock()
			return ctx.Err()
		case in, ok := <-lines:
			if !ok {
				return nil
			}
			var code int
			if _, err := fmt.Sscanf(in, "exit %d", &code); err == nil {
				req.Stdout.Write([]byte("bye\r\n"))
				return exitErr(code)
			}
			req.Stdout.Write([]byte("<" + in + ">"))
		}
	}
}

func (f *fakeCluster) wasCanceled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canceled
}

type fakeAudit struct {
	mu   sync.Mutex
	rows map[int64]*store.AuditEntry
	fail bool
}

func (a *fakeAudit) StartAudit(_ context.Context, e store.AuditEntry) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return 0, errors.New("database down")
	}
	if a.rows == nil {
		a.rows = map[int64]*store.AuditEntry{}
	}
	e.ID = int64(len(a.rows) + 1)
	e.StartedAt = time.Now()
	a.rows[e.ID] = &e
	return e.ID, nil
}

func (a *fakeAudit) EndAudit(_ context.Context, id int64, code *int, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.rows[id]
	now := time.Now()
	e.EndedAt, e.ExitCode, e.EndReason = &now, code, reason
	return nil
}

func (a *fakeAudit) get(id int64) store.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return *a.rows[id]
}

type fakeWaker struct {
	mu    sync.Mutex
	keeps int
}

func (w *fakeWaker) Wake(context.Context, store.Deployment) error { return nil }
func (w *fakeWaker) KeepAwake(store.Deployment) {
	w.mu.Lock()
	w.keeps++
	w.mu.Unlock()
}

// ---- harness ----

type harness struct {
	svc     *Service
	cluster *fakeCluster
	audit   *fakeAudit
	results chan Result
	conn    *wsock.Conn
	cancel  context.CancelFunc
}

func start(t *testing.T, cfg Config, cluster *fakeCluster, tweak func(*Service)) *harness {
	t.Helper()
	h := &harness{cluster: cluster, audit: &fakeAudit{}, results: make(chan Result, 1)}
	h.svc = &Service{Cluster: cluster, Audit: h.audit, Config: cfg, tick: 5 * time.Millisecond}
	if tweak != nil {
		tweak(h.svc)
	}
	srvCtx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsock.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		h.results <- h.svc.Run(srvCtx, conn, Session{
			App: store.App{ID: 1, Name: "blog"}, Deployment: store.Deployment{ID: 7, AppName: "blog"},
			Namespace: "app-blog", Pod: Pod{Name: "d-abc-1", Process: "web", Ready: true},
			UserID: 5, Actor: "ayse", Size: Size{Cols: 80, Rows: 24},
		})
	}))
	t.Cleanup(func() { cancel(); srv.Close() })
	conn, _, err := wsock.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	h.conn = conn
	return h
}

// read returns the next message; text messages are decoded.
func (h *harness) read(t *testing.T) (int, []byte, message) {
	t.Helper()
	h.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	op, data, err := h.conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m message
	if op == wsock.TextMessage {
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
	}
	return op, data, m
}

func (h *harness) expectText(t *testing.T, typ string) message {
	t.Helper()
	for {
		op, data, m := h.read(t)
		if op == wsock.BinaryMessage {
			continue // output before the status
		}
		if m.Type != typ {
			t.Fatalf("got %s, want %s", data, typ)
		}
		return m
	}
}

func (h *harness) expectClose(t *testing.T, code int) {
	t.Helper()
	h.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := h.conn.ReadMessage()
		if err == nil {
			continue
		}
		if !wsock.IsClose(err, code) {
			t.Fatalf("got %v, want close %d", err, code)
		}
		return
	}
}

func (h *harness) result(t *testing.T) Result {
	t.Helper()
	select {
	case r := <-h.results:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end")
		return Result{}
	}
}

func (h *harness) send(t *testing.T, s string) {
	t.Helper()
	if err := h.conn.WriteMessage(wsock.BinaryMessage, []byte(s)); err != nil {
		t.Fatal(err)
	}
}

// ---- tests ----

func TestSessionEchoResizeAndExitCode(t *testing.T) {
	h := start(t, Config{}, &fakeCluster{}, nil)
	ready := h.expectText(t, "ready")
	if ready.Pod != "d-abc-1" || ready.Process != "web" || ready.IdleSeconds != 600 || ready.MaxSeconds != 3600 {
		t.Fatalf("ready: %+v", ready)
	}
	h.send(t, "ls")
	op, data, _ := h.read(t)
	if op != wsock.BinaryMessage || string(data) != "<ls>" {
		t.Fatalf("echo: %d %q", op, data)
	}
	resize, _ := json.Marshal(map[string]any{"type": "resize", "cols": 120, "rows": 40})
	h.conn.WriteMessage(wsock.TextMessage, resize)
	// Out-of-range sizes are ignored.
	bad, _ := json.Marshal(map[string]any{"type": "resize", "cols": 0, "rows": 40})
	h.conn.WriteMessage(wsock.TextMessage, bad)
	h.send(t, "pwd")
	if _, data, _ := h.read(t); string(data) != "<pwd>" {
		t.Fatalf("echo 2: %q", data)
	}
	h.send(t, "exit 3")
	if _, data, _ := h.read(t); !bytes.Equal(data, []byte("bye\r\n")) {
		t.Fatalf("output before exit: %q", data)
	}
	m := h.expectText(t, "exit")
	if m.Code == nil || *m.Code != 3 {
		t.Fatalf("exit message %+v", m)
	}
	h.expectClose(t, wsock.CloseNormal)
	r := h.result(t)
	if r.Reason != ReasonExit || r.ExitCode == nil || *r.ExitCode != 3 {
		t.Fatalf("result %+v", r)
	}
	row := h.audit.get(r.AuditID)
	if row.Action != store.AuditTerminal || row.AppID != 1 || *row.DeploymentID != 7 || *row.UserID != 5 ||
		row.Actor != "ayse" || row.Pod != "d-abc-1" || row.Process != "web" ||
		row.EndedAt == nil || *row.ExitCode != 3 || row.EndReason != ReasonExit {
		t.Fatalf("audit row %+v", row)
	}

	h.cluster.mu.Lock()
	defer h.cluster.mu.Unlock()
	req := h.cluster.reqs[0]
	if req.Namespace != "app-blog" || req.Pod != "d-abc-1" || req.Command[0] != "/bin/sh" {
		t.Fatalf("exec request %+v", req)
	}
	if len(h.cluster.sizes) < 2 || h.cluster.sizes[0] != (Size{80, 24}) || h.cluster.sizes[len(h.cluster.sizes)-1] != (Size{120, 40}) {
		t.Fatalf("sizes %v", h.cluster.sizes)
	}
}

func TestSessionIdleTimeout(t *testing.T) {
	h := start(t, Config{IdleTimeout: 150 * time.Millisecond, MaxDuration: 10 * time.Second}, &fakeCluster{}, nil)
	h.expectText(t, "ready")
	// Typing keeps the session open past the idle timeout...
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		h.send(t, "x")
		if _, data, _ := h.read(t); string(data) != "<x>" {
			t.Fatalf("echo %q", data)
		}
		time.Sleep(40 * time.Millisecond)
	}
	// ...and stopping closes it.
	began := time.Now()
	m := h.expectText(t, "closed")
	if m.Reason != ReasonIdle || !strings.Contains(m.Message, "işlem yapılmadığı") {
		t.Fatalf("closed message %+v", m)
	}
	h.expectClose(t, CodeIdle)
	if waited := time.Since(began); waited > 2*time.Second {
		t.Fatalf("idle close after %v", waited)
	}
	r := h.result(t)
	row := h.audit.get(r.AuditID)
	if row.EndReason != ReasonIdle || row.ExitCode != nil || row.EndedAt == nil {
		t.Fatalf("audit row %+v", row)
	}
	if !h.cluster.wasCanceled() {
		t.Fatal("exec was not cancelled")
	}
}

func TestSessionMaxDuration(t *testing.T) {
	h := start(t, Config{IdleTimeout: 10 * time.Second, MaxDuration: 200 * time.Millisecond}, &fakeCluster{}, nil)
	h.expectText(t, "ready")
	stop := make(chan struct{})
	go func() { // keep typing: input does not extend the maximum
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				h.conn.WriteMessage(wsock.BinaryMessage, []byte("y"))
			}
		}
	}()
	defer close(stop)
	m := h.expectText(t, "closed")
	if m.Reason != ReasonMaxDuration {
		t.Fatalf("closed message %+v", m)
	}
	h.expectClose(t, CodeMaxDuration)
	if r := h.result(t); r.Reason != ReasonMaxDuration || h.audit.get(r.AuditID).EndReason != ReasonMaxDuration {
		t.Fatalf("result %+v", r)
	}
}

func TestSessionNoShell(t *testing.T) {
	h := start(t, Config{}, &fakeCluster{noShell: true}, nil)
	h.expectText(t, "ready")
	m := h.expectText(t, "error")
	if m.Reason != ReasonNoShell || !strings.HasPrefix(m.Message, "Bu imajda kabuk yok") {
		t.Fatalf("error message %+v", m)
	}
	h.expectClose(t, CodeNoShell)
	r := h.result(t)
	if r.Reason != ReasonNoShell || h.audit.get(r.AuditID).EndReason != ReasonNoShell {
		t.Fatalf("result %+v", r)
	}
}

func TestSessionExecError(t *testing.T) {
	h := start(t, Config{}, &fakeCluster{failure: errors.New("pods \"d-abc-1\" not found")}, nil)
	h.expectText(t, "ready")
	m := h.expectText(t, "error")
	if m.Reason != ReasonError || !strings.Contains(m.Message, "not found") {
		t.Fatalf("error message %+v", m)
	}
	h.expectClose(t, wsock.CloseInternalError)
	h.result(t)
}

func TestSessionClientCloses(t *testing.T) {
	w := &fakeWaker{}
	h := start(t, Config{}, &fakeCluster{}, func(s *Service) {
		s.Waker, s.keepAwakeEvery, s.pingEvery = w, 20*time.Millisecond, 20*time.Millisecond
	})
	h.expectText(t, "ready")
	// Pings arrive and are answered by the client's reads.
	time.Sleep(100 * time.Millisecond)
	h.conn.WriteClose(wsock.CloseNormal, "")
	r := h.result(t)
	if r.Reason != ReasonClientClosed || h.audit.get(r.AuditID).EndReason != ReasonClientClosed {
		t.Fatalf("result %+v", r)
	}
	if !h.cluster.wasCanceled() {
		t.Fatal("exec was not cancelled when the browser left")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.keeps < 2 {
		t.Fatalf("KeepAwake called %d times", w.keeps)
	}
}

func TestSessionShutdown(t *testing.T) {
	h := start(t, Config{}, &fakeCluster{}, nil)
	h.expectText(t, "ready")
	h.cancel()
	m := h.expectText(t, "closed")
	if m.Reason != ReasonShutdown {
		t.Fatalf("closed %+v", m)
	}
	h.expectClose(t, wsock.CloseGoingAway)
	if r := h.result(t); r.Reason != ReasonShutdown {
		t.Fatalf("result %+v", r)
	}
}

func TestSessionAuditFailureRefuses(t *testing.T) {
	cluster := &fakeCluster{}
	hh := start(t, Config{}, cluster, func(s *Service) { s.Audit = &fakeAudit{fail: true} })
	m := hh.expectText(t, "error")
	if m.Reason != ReasonError {
		t.Fatalf("got %+v", m)
	}
	hh.expectClose(t, wsock.CloseInternalError)
	hh.result(t)
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	if len(cluster.reqs) != 0 {
		t.Fatal("exec ran without an audit record")
	}
}

func TestAcquireLimit(t *testing.T) {
	s := &Service{Config: Config{MaxPerUser: 2}}
	r1, ok1 := s.Acquire("u1")
	_, ok2 := s.Acquire("u1")
	_, ok3 := s.Acquire("u1")
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("acquire: %v %v %v", ok1, ok2, ok3)
	}
	if _, ok := s.Acquire("u2"); !ok {
		t.Fatal("limit is per user")
	}
	r1()
	r1() // releasing twice frees one slot only
	if s.Active("u1") != 1 {
		t.Fatalf("active %d", s.Active("u1"))
	}
	if _, ok := s.Acquire("u1"); !ok {
		t.Fatal("slot not freed")
	}
	if (&Service{}).MaxPerUser() != DefaultMaxPerUser {
		t.Fatal("default limit")
	}
}

func TestChoose(t *testing.T) {
	t0 := time.Now()
	pods := []Pod{
		{Name: "web-old", Process: "web", Ready: true, Created: t0.Add(-time.Hour)},
		{Name: "web-new", Process: "web", Ready: true, Created: t0},
		{Name: "web-starting", Process: "web", Ready: false, Created: t0.Add(time.Minute)},
		{Name: "worker-1", Process: "worker", Ready: true, Created: t0},
		{Name: "cron-1", Process: "cleanup", Ready: false, Created: t0},
	}
	for _, c := range []struct {
		process, pod, want string
		err                bool
	}{
		{"", "", "web-new", false},
		{"web", "", "web-new", false},
		{"worker", "", "worker-1", false},
		{"", "web-old", "web-old", false},
		{"", "web-starting", "", true}, // not ready
		{"", "other-app-pod", "", true},
		{"cleanup", "", "", true},
		{"missing", "", "", true},
	} {
		got, err := Choose(pods, c.process, c.pod)
		if (err != nil) != c.err || (err == nil && got.Name != c.want) {
			t.Errorf("Choose(%q, %q) = %v, %v; want %q", c.process, c.pod, got.Name, err, c.want)
		}
		if err != nil && !errors.Is(err, ErrNoPod) {
			t.Errorf("error %v is not ErrNoPod", err)
		}
	}
	// Without a web process the first process with a ready pod is used.
	got, err := Choose(pods[3:], "", "")
	if err != nil || got.Name != "worker-1" {
		t.Fatalf("web-less: %v %v", got, err)
	}
	if p := Processes(pods); strings.Join(p, ",") != "web,cleanup,worker" {
		t.Fatalf("Processes = %v", p)
	}
}

func TestReadAuth(t *testing.T) {
	results := make(chan error, 1)
	tokens := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsock.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		tok, err := ReadAuth(conn, 500*time.Millisecond)
		tokens <- tok
		results <- err
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	c, _, err := wsock.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.WriteMessage(wsock.TextMessage, []byte(`{"type":"auth","csrf":"tok"}`))
	if err := <-results; err != nil || <-tokens != "tok" {
		t.Fatalf("auth: %v", err)
	}
	c.Close()

	// Keystrokes first are refused.
	c, _, _ = wsock.Dial(context.Background(), url, nil)
	c.WriteMessage(wsock.BinaryMessage, []byte("ls\r"))
	if err := <-results; err == nil {
		t.Fatal("binary first message accepted")
	}
	<-tokens
	c.Close()

	// Silence times out.
	c, _, _ = wsock.Dial(context.Background(), url, nil)
	if err := <-results; err == nil {
		t.Fatal("no auth message accepted")
	}
	<-tokens
	c.Close()
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Minute: "10 dakika", time.Hour: "60 dakika", 90 * time.Second: "90 saniye",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("%v: %q", d, got)
		}
	}
}
