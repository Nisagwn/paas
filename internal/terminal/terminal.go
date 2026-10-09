// Package terminal runs browser terminal sessions (Faz 27): a shell in a
// running pod of a deployment, attached to a WebSocket (internal/wsock).
//
// The browser and the server speak a small protocol over one WebSocket:
//
//	browser → server  binary frame        keystrokes (stdin of the shell)
//	                  text {"type":"resize","cols":120,"rows":32}
//	                  text {"type":"auth","csrf":"…"}  (first message, when the
//	                       token is not in the URL; handled by internal/web)
//	server → browser  binary frame        terminal output (stdout; with a TTY
//	                                      stderr is merged into it)
//	                  text {"type":"ready"|"exit"|"closed"|"error", …}
//
// Safety: a session ends after IdleTimeout without input from the browser
// and, in any case, after MaxDuration; a user has at most MaxPerUser
// sessions at once. Every session is recorded in the audit log (who, app,
// deployment, pod, start, end, exit code) — never the keystrokes or output.
package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/wsock"
)

// Defaults of Config.
const (
	DefaultIdleTimeout = 10 * time.Minute
	DefaultMaxDuration = 60 * time.Minute
	DefaultMaxPerUser  = 3
	// WebProcess is the process a session opens in unless another is chosen.
	WebProcess = "web"
)

// DefaultShell starts bash when the image has it, else sh. Exec runs
// /bin/sh first, so an image without any shell fails right away and the
// browser is told so (ErrNoShell).
var DefaultShell = []string{"/bin/sh", "-c",
	`export TERM=xterm-256color; if command -v bash >/dev/null 2>&1; then exec bash; fi; exec /bin/sh`}

// Close codes sent to the browser (4000–4999 are application codes).
const (
	CodeIdle        = 4000
	CodeMaxDuration = 4001
	CodeForbidden   = 4003
	CodeNoShell     = 4004
	CodeNoPod       = 4009
	CodeTooMany     = 4029
)

// End reasons stored in the audit log.
const (
	ReasonExit         = "exit"
	ReasonIdle         = "idle"
	ReasonMaxDuration  = "max_duration"
	ReasonClientClosed = "client_closed"
	ReasonNoShell      = "no_shell"
	ReasonError        = "error"
	ReasonShutdown     = "shutdown"
)

// NoShellMessage is shown when the image has no /bin/sh (e.g. distroless Go
// images).
const NoShellMessage = "Bu imajda kabuk yok (/bin/sh bulunamadı). Distroless ya da scratch tabanlı imajlarda, örneğin " +
	"paas'ın Go imajlarında, terminal açılamaz; uygulamanı incelemek için çalışma loglarını kullan."

var (
	// ErrNoShell: the container has no /bin/sh.
	ErrNoShell = errors.New("terminal: the image has no shell")
	// ErrNoPod: no running pod matches the request.
	ErrNoPod = errors.New("terminal: no running pod")
)

// Pod is a pod of a deployment that a session may open in.
type Pod struct {
	Name string
	// Process is "web" or the worker / cron name (Faz 20).
	Process string
	// Ready: running and its container is ready (cron runs: running).
	Ready   bool
	Phase   string
	Created time.Time
}

// Size is a terminal size in character cells.
type Size struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// SizeQueue delivers resize events to the executor; Next blocks until the
// next size and returns false once the session ends.
type SizeQueue interface {
	Next() (Size, bool)
}

// ExecRequest is one exec into a pod with a TTY.
type ExecRequest struct {
	Namespace, Pod, Container string
	Command                   []string
	Stdin                     io.Reader
	Stdout                    io.Writer
	Resize                    SizeQueue
}

// Cluster lists a deployment's pods and runs commands in them
// (deploy.Terminal; tests use a fake).
type Cluster interface {
	Pods(ctx context.Context, d store.Deployment) ([]Pod, error)
	// Exec runs until the command exits (nil, or an error with
	// ExitStatus() int), ctx ends, or the stream fails. An image without
	// /bin/sh yields an error wrapping ErrNoShell.
	Exec(ctx context.Context, req ExecRequest) error
}

// Audit records sessions (store.Store).
type Audit interface {
	StartAudit(ctx context.Context, e store.AuditEntry) (int64, error)
	EndAudit(ctx context.Context, id int64, exitCode *int, reason string) error
}

// Waker wakes a sleeping deployment (the Faz 11 scaler) and keeps one with
// an open session from being put to sleep.
type Waker interface {
	Wake(ctx context.Context, d store.Deployment) error
	KeepAwake(d store.Deployment)
}

// Config holds the limits; zero values take the defaults.
type Config struct {
	IdleTimeout time.Duration
	MaxDuration time.Duration
	MaxPerUser  int
	Shell       []string
}

// Service runs sessions.
type Service struct {
	Cluster Cluster
	Audit   Audit
	// Waker is optional (scale to zero off): sleeping deployments then only
	// get a message.
	Waker  Waker
	Config Config
	Log    *slog.Logger

	// Test hooks: tick is the timer resolution, pingEvery the keepalive
	// interval, keepAwakeEvery how often an open session touches the scaler.
	tick, pingEvery, keepAwakeEvery time.Duration

	mu     sync.Mutex
	active map[string]int
}

func (s *Service) idle() time.Duration {
	if s.Config.IdleTimeout > 0 {
		return s.Config.IdleTimeout
	}
	return DefaultIdleTimeout
}

func (s *Service) maxDuration() time.Duration {
	if s.Config.MaxDuration > 0 {
		return s.Config.MaxDuration
	}
	return DefaultMaxDuration
}

// MaxPerUser is the concurrent session limit.
func (s *Service) MaxPerUser() int {
	if s.Config.MaxPerUser > 0 {
		return s.Config.MaxPerUser
	}
	return DefaultMaxPerUser
}

// IdleTimeout and MaxDuration are the effective limits (for the page).
func (s *Service) IdleTimeout() time.Duration { return s.idle() }
func (s *Service) MaxDuration() time.Duration { return s.maxDuration() }

// Acquire reserves one of user's session slots; ok is false at the limit.
// release must be called once the session ends.
func (s *Service) Acquire(user string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		s.active = map[string]int{}
	}
	if s.active[user] >= s.MaxPerUser() {
		return nil, false
	}
	s.active[user]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.active[user]--; s.active[user] <= 0 {
				delete(s.active, user)
			}
		})
	}, true
}

// Active returns user's open sessions.
func (s *Service) Active(user string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[user]
}

// Choose picks the pod a session opens in: the named pod (it must be one of
// pods and ready), else the newest ready pod of process ("" means web, or,
// for an app without a web process, the first process that has one).
func Choose(pods []Pod, process, pod string) (Pod, error) {
	if pod != "" {
		for _, p := range pods {
			if p.Name == pod && p.Ready {
				return p, nil
			}
		}
		return Pod{}, fmt.Errorf("%w: pod %s", ErrNoPod, pod)
	}
	ready := map[string][]Pod{}
	for _, p := range pods {
		if p.Ready {
			ready[p.Process] = append(ready[p.Process], p)
		}
	}
	if process == "" {
		process = WebProcess
		if len(ready[WebProcess]) == 0 {
			for _, name := range Processes(pods) {
				if len(ready[name]) > 0 {
					process = name
					break
				}
			}
		}
	}
	cands := ready[process]
	if len(cands) == 0 {
		return Pod{}, fmt.Errorf("%w: process %s", ErrNoPod, process)
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Created.After(cands[j].Created) })
	return cands[0], nil
}

// Processes lists the process names of pods: web first, then by name.
func Processes(pods []Pod) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range pods {
		if !seen[p.Process] {
			seen[p.Process] = true
			out = append(out, p.Process)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i] == WebProcess) != (out[j] == WebProcess) {
			return out[i] == WebProcess
		}
		return out[i] < out[j]
	})
	return out
}

// Session describes one session to run.
type Session struct {
	App        store.App
	Deployment store.Deployment
	Namespace  string
	Pod        Pod
	// UserID 0 is the admin principal.
	UserID int64
	Actor  string
	// Size is the browser's terminal size when it connected (optional).
	Size Size
}

// Result is how a session ended.
type Result struct {
	AuditID  int64
	ExitCode *int
	Reason   string
}

// message is a text frame to the browser.
type message struct {
	Type        string `json:"type"`
	Pod         string `json:"pod,omitempty"`
	Process     string `json:"process,omitempty"`
	IdleSeconds int    `json:"idle_seconds,omitempty"`
	MaxSeconds  int    `json:"max_seconds,omitempty"`
	Code        *int   `json:"code,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Message     string `json:"message,omitempty"`
}

// clientMessage is a text frame from the browser.
type clientMessage struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	Data string `json:"data"`
	CSRF string `json:"csrf"`
}

func send(conn *wsock.Conn, m message) error {
	b, _ := json.Marshal(m)
	return conn.WriteMessage(wsock.TextMessage, b)
}

// Refuse tells the browser why no session starts, closes with code and
// waits briefly for the browser's close.
func Refuse(conn *wsock.Conn, code int, reason, text string) {
	send(conn, message{Type: "error", Reason: reason, Message: text})
	closeAndDrain(conn, code, reason)
}

// closeAndDrain sends a close frame, reads until the browser answers (or a
// short deadline passes) and closes the connection.
func closeAndDrain(conn *wsock.Conn, code int, reason string) {
	conn.WriteClose(code, reason)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	conn.Close()
}

// ReadAuth waits up to timeout for the browser's first message, which must
// be {"type":"auth","csrf":…}, and returns the token. Keystrokes sent
// before it are an error: nothing reaches a pod before the check.
func ReadAuth(conn *wsock.Conn, timeout time.Duration) (string, error) {
	conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})
	op, data, err := conn.ReadMessage()
	if err != nil {
		return "", err
	}
	var m clientMessage
	if op != wsock.TextMessage || json.Unmarshal(data, &m) != nil || m.Type != "auth" {
		return "", errors.New("terminal: first message must be auth")
	}
	return m.CSRF, nil
}

// outWriter sends the shell's output as binary frames.
type outWriter struct{ conn *wsock.Conn }

func (w outWriter) Write(p []byte) (int, error) {
	if err := w.conn.WriteMessage(wsock.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// exitCode extracts the shell's exit status from Exec's error.
func exitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var ee interface{ ExitStatus() int }
	if errors.As(err, &ee) {
		return ee.ExitStatus(), true
	}
	return 0, false
}

// Run runs one session on an upgraded connection until the shell exits, the
// browser leaves, a limit is reached or ctx ends; it closes conn. The caller
// holds a slot from Acquire.
func (s *Service) Run(ctx context.Context, conn *wsock.Conn, sess Session) Result {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	res := Result{}
	auditID, err := s.Audit.StartAudit(ctx, store.AuditEntry{
		Action: store.AuditTerminal, AppID: sess.App.ID, DeploymentID: &sess.Deployment.ID,
		UserID: &sess.UserID, Actor: sess.Actor, Process: sess.Pod.Process, Pod: sess.Pod.Name,
	})
	if err != nil {
		log.Error("terminal: audit start", "app", sess.App.Name, "err", err)
		Refuse(conn, wsock.CloseInternalError, ReasonError, "Oturum kaydı oluşturulamadı; terminal açılmadı.")
		res.Reason = ReasonError
		return res
	}
	res.AuditID = auditID
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.Audit.EndAudit(ctx, auditID, res.ExitCode, res.Reason); err != nil {
			log.Error("terminal: audit end", "id", auditID, "err", err)
		}
		log.Info("terminal: session ended", "app", sess.App.Name, "deployment", sess.Deployment.ID,
			"pod", sess.Pod.Name, "actor", sess.Actor, "reason", res.Reason, "audit", auditID)
	}()
	log.Info("terminal: session started", "app", sess.App.Name, "deployment", sess.Deployment.ID,
		"pod", sess.Pod.Name, "process", sess.Pod.Process, "actor", sess.Actor, "audit", auditID)

	idle, maxDur := s.idle(), s.maxDuration()
	if err := send(conn, message{Type: "ready", Pod: sess.Pod.Name, Process: sess.Pod.Process,
		IdleSeconds: int(idle.Seconds()), MaxSeconds: int(maxDur.Seconds())}); err != nil {
		conn.Close()
		res.Reason = ReasonClientClosed
		return res
	}

	shell := s.Config.Shell
	if len(shell) == 0 {
		shell = DefaultShell
	}
	queue := newSizeQueue()
	if sess.Size.Cols > 0 && sess.Size.Rows > 0 {
		queue.Set(sess.Size)
	}
	stdinR, stdinW := io.Pipe()
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()

	execDone := make(chan error, 1)
	go func() {
		execDone <- s.Cluster.Exec(execCtx, ExecRequest{
			Namespace: sess.Namespace, Pod: sess.Pod.Name, Command: shell,
			Stdin: stdinR, Stdout: outWriter{conn}, Resize: queue,
		})
	}()

	var lastInput atomic.Int64
	lastInput.Store(time.Now().UnixNano())
	readDone := make(chan error, 1)
	go func() {
		for {
			op, data, err := conn.ReadMessage()
			if err != nil {
				readDone <- err
				return
			}
			switch op {
			case wsock.BinaryMessage:
				lastInput.Store(time.Now().UnixNano())
				if _, err := stdinW.Write(data); err != nil {
					// The shell is gone; keep reading for the close.
					continue
				}
			case wsock.TextMessage:
				var m clientMessage
				if json.Unmarshal(data, &m) != nil {
					continue
				}
				switch m.Type {
				case "resize":
					if m.Cols > 0 && m.Rows > 0 && m.Cols <= 1000 && m.Rows <= 1000 {
						lastInput.Store(time.Now().UnixNano())
						queue.Set(Size{Cols: uint16(m.Cols), Rows: uint16(m.Rows)})
					}
				case "stdin":
					lastInput.Store(time.Now().UnixNano())
					stdinW.Write([]byte(m.Data))
				}
			}
		}
	}()

	tick := s.tick
	if tick <= 0 {
		tick = min(max(min(idle, maxDur)/20, 10*time.Millisecond), time.Second)
	}
	pingEvery := s.pingEvery
	if pingEvery <= 0 {
		pingEvery = 30 * time.Second
	}
	keepAwakeEvery := s.keepAwakeEvery
	if keepAwakeEvery <= 0 {
		keepAwakeEvery = time.Minute
	}
	if s.Waker != nil {
		s.Waker.KeepAwake(sess.Deployment)
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	start := time.Now()
	lastPing, lastAwake := start, start

	var (
		code       = wsock.CloseNormal
		final      message
		clientGone bool
		execEnded  bool
	)
loop:
	for {
		select {
		case err := <-execDone:
			execEnded = true
			switch c, ok := exitCode(err); {
			case ok:
				res.ExitCode, res.Reason = &c, ReasonExit
				final = message{Type: "exit", Code: &c}
			case errors.Is(err, ErrNoShell):
				res.Reason, code = ReasonNoShell, CodeNoShell
				final = message{Type: "error", Reason: ReasonNoShell, Message: NoShellMessage}
			case ctx.Err() != nil:
				res.Reason, code = ReasonShutdown, wsock.CloseGoingAway
				final = message{Type: "closed", Reason: ReasonShutdown, Message: "Sunucu yeniden başlatılıyor; oturum kapatıldı."}
			default:
				log.Warn("terminal: exec failed", "pod", sess.Pod.Name, "err", err)
				res.Reason, code = ReasonError, wsock.CloseInternalError
				final = message{Type: "error", Reason: ReasonError, Message: "Pod'a bağlanılamadı ya da bağlantı koptu: " + err.Error()}
			}
			break loop
		case <-readDone:
			clientGone = true
			res.Reason = ReasonClientClosed
			break loop
		case <-ctx.Done():
			res.Reason, code = ReasonShutdown, wsock.CloseGoingAway
			final = message{Type: "closed", Reason: ReasonShutdown, Message: "Sunucu yeniden başlatılıyor; oturum kapatıldı."}
			break loop
		case now := <-ticker.C:
			if now.Sub(time.Unix(0, lastInput.Load())) >= idle {
				res.Reason, code = ReasonIdle, CodeIdle
				final = message{Type: "closed", Reason: ReasonIdle,
					Message: fmt.Sprintf("%s boyunca işlem yapılmadığı için oturum kapatıldı.", humanDuration(idle))}
				break loop
			}
			if now.Sub(start) >= maxDur {
				res.Reason, code = ReasonMaxDuration, CodeMaxDuration
				final = message{Type: "closed", Reason: ReasonMaxDuration,
					Message: fmt.Sprintf("Bir oturum en fazla %s sürebilir; oturum kapatıldı.", humanDuration(maxDur))}
				break loop
			}
			if now.Sub(lastPing) >= pingEvery {
				lastPing = now
				conn.WriteMessage(wsock.PingMessage, nil)
			}
			if s.Waker != nil && now.Sub(lastAwake) >= keepAwakeEvery {
				lastAwake = now
				s.Waker.KeepAwake(sess.Deployment)
			}
		}
	}

	// Stop the shell: the exec stream is cancelled and stdin ends.
	cancelExec()
	stdinW.Close() // EOF for the exec's stdin copy (closing the reader makes client-go log an error)
	queue.Close()
	if !execEnded {
		select {
		case <-execDone:
		case <-time.After(5 * time.Second):
			log.Warn("terminal: exec did not stop", "pod", sess.Pod.Name)
		}
	}
	if clientGone {
		conn.Close()
		return res
	}
	send(conn, final)
	conn.WriteClose(code, res.Reason)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	select {
	case <-readDone:
	case <-time.After(3 * time.Second):
	}
	conn.Close()
	return res
}

// humanDuration renders 10m as "10 dakika", 1h as "60 dakika", 90s as "90 saniye".
func humanDuration(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d dakika", int(d/time.Minute))
	}
	if d >= time.Second {
		return fmt.Sprintf("%d saniye", int(d.Round(time.Second)/time.Second))
	}
	return d.String()
}

// sizeQueue keeps only the latest size: a slow executor never blocks the
// browser's reader, and stale sizes are dropped.
type sizeQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	next   *Size
	closed bool
}

func newSizeQueue() *sizeQueue {
	q := &sizeQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *sizeQueue) Set(s Size) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.next = &s
	q.cond.Signal()
}

func (q *sizeQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

func (q *sizeQueue) Next() (Size, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.next == nil && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return Size{}, false
	}
	s := *q.next
	q.next = nil
	return s, true
}
