package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/terminal"
	"github.com/nisagwn/paas/internal/web"
	"github.com/nisagwn/paas/internal/wsock"
)

// Faz 27: browser terminal pages, WebSocket authorization and audit list.

// termCluster has one ready web pod and echoes stdin until "exit".
type termCluster struct {
	mu    sync.Mutex
	execs []terminal.ExecRequest
}

func (c *termCluster) Pods(context.Context, store.Deployment) ([]terminal.Pod, error) {
	return []terminal.Pod{
		{Name: "d-0000000-abc", Process: "web", Ready: true, Created: time.Now()},
		{Name: "d-0000000-w-worker-xyz", Process: "worker", Ready: true, Created: time.Now()},
	}, nil
}

type exitStatus int

func (e exitStatus) Error() string   { return "exit" }
func (e exitStatus) ExitStatus() int { return int(e) }

func (c *termCluster) Exec(ctx context.Context, req terminal.ExecRequest) error {
	c.mu.Lock()
	c.execs = append(c.execs, req)
	c.mu.Unlock()
	go func() {
		for _, ok := req.Resize.Next(); ok; _, ok = req.Resize.Next() {
		}
	}()
	buf := make([]byte, 256)
	for {
		n, err := req.Stdin.Read(buf)
		if err != nil {
			return ctx.Err()
		}
		if strings.HasPrefix(string(buf[:n]), "exit") {
			return exitStatus(0)
		}
		req.Stdout.Write([]byte("echo:" + string(buf[:n])))
	}
}

type termWaker struct {
	st    *store.Store
	woken []int64
}

func (w *termWaker) Wake(ctx context.Context, d store.Deployment) error {
	w.woken = append(w.woken, d.ID)
	return w.st.SetSleeping(ctx, d.ID, false)
}
func (w *termWaker) KeepAwake(store.Deployment) {}

type termSetup struct {
	*f20
	cluster *termCluster
	waker   *termWaker
	other   *ui
	ready   store.Deployment
}

func setupTerminal(t *testing.T, on bool) *termSetup {
	ts := &termSetup{cluster: &termCluster{}}
	ts.f20 = setupF20(t, func(s *web.Server) {
		if on {
			ts.waker = &termWaker{st: s.Store}
			s.Terminal = &terminal.Service{Cluster: ts.cluster, Audit: s.Store, Waker: ts.waker,
				Config: terminal.Config{MaxPerUser: 1}}
		}
	})
	ctx := context.Background()
	dave, _ := ts.st.UpsertUser(ctx, 3, "dave", "", "")
	ts.st.CreateTeam(ctx, "ops", "Ops", dave.ID)
	ts.other = ts.as(dave.ID)
	d, _, _ := ts.st.EnqueueDeployment(ctx, ts.app.ID, sha(1), "main", "one")
	if err := ts.st.MarkReady(ctx, d, []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}}); err != nil {
		t.Fatal(err)
	}
	ts.ready, _ = ts.st.GetDeployment(ctx, d.ID)
	return ts
}

func termPath(id int64) string { return fmt.Sprintf("/apps/blog/deployments/%d/terminal", id) }

// dial opens the terminal WebSocket as c. origin "self" is the server's own.
func (ts *termSetup) dial(c *ui, path, origin string) (*wsock.Conn, *http.Response, error) {
	h := http.Header{}
	if origin == "self" {
		origin = c.srv.URL
	}
	if origin != "" {
		h.Set("Origin", origin)
	}
	su, _ := url.Parse(c.srv.URL)
	var cookies []string
	for _, ck := range c.client.Jar.Cookies(su) {
		cookies = append(cookies, ck.Name+"="+ck.Value)
	}
	h.Set("Cookie", strings.Join(cookies, "; "))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return wsock.Dial(ctx, "ws"+strings.TrimPrefix(c.srv.URL, "http")+path, h)
}

func readJSON(t *testing.T, c *wsock.Conn) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		op, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if op != wsock.TextMessage {
			continue
		}
		var m map[string]any
		json.Unmarshal(data, &m)
		return m
	}
}

func expectCloseCode(t *testing.T, c *wsock.Conn, code int) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		if !wsock.IsClose(err, code) {
			t.Fatalf("got %v, want close %d", err, code)
		}
		return
	}
}

func sendAuth(c *wsock.Conn, token string) {
	b, _ := json.Marshal(map[string]string{"type": "auth", "csrf": token})
	c.WriteMessage(wsock.TextMessage, b)
}

func TestTerminalPageAndButtons(t *testing.T) {
	ts := setupTerminal(t, true)
	path := termPath(ts.ready.ID)

	resp, err := ts.owner.client.Get(ts.srv0() + path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	host := strings.TrimPrefix(ts.srv0(), "http://")
	for _, want := range []string{"script-src 'nonce-", "https://unpkg.com", "style-src 'self' 'unsafe-inline' https://unpkg.com",
		"connect-src 'self' ws://" + host + " wss://" + host, "frame-ancestors 'none'", "default-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP too loose: %q", csp)
	}
	nonce := csp[strings.Index(csp, "'nonce-")+7:]
	nonce = nonce[:strings.Index(nonce, "'")]
	// html/template escapes "+" in attributes; the browser decodes it back.
	nonce = strings.ReplaceAll(nonce, "+", "&#43;")
	mustContain(t, "terminal page", string(body),
		`src="https://unpkg.com/@xterm/xterm@5.5.0/lib/xterm.js" integrity="sha384-`,
		`src="https://unpkg.com/@xterm/addon-fit@0.10.0/lib/addon-fit.js" integrity="sha384-`,
		`href="https://unpkg.com/@xterm/xterm@5.5.0/css/xterm.css" integrity="sha384-`,
		`nonce="`+nonce+`"`, "data-csrf=", "Yeniden bağlan", "d-0000000-abc", "worker · d-0000000-w-worker-xyz",
		"10 dakika işlem yapılmazsa", "60 dakika")
	// Every inline script carries the nonce.
	if strings.Count(string(body), "<script") != strings.Count(string(body), `nonce="`+nonce+`"`) {
		t.Error("a script without the page nonce")
	}

	// Buttons: members on running deployments; never viewers.
	for _, p := range []string{"/apps/blog/deployments", fmt.Sprintf("/deployments/%d", ts.ready.ID)} {
		_, b, _ := ts.owner.get(p)
		mustContain(t, "owner "+p, b, `href="`+path+`"`)
		_, b, _ = ts.viewer.get(p)
		mustNotContain(t, "viewer "+p, b, `href="`+path+`"`)
	}
	_, b, _ := ts.owner.get("/apps/blog/settings")
	mustContain(t, "settings", b, "Terminal oturumları", "Henüz terminal oturumu açılmadı")
	_, b, _ = ts.owner.get("/apps/blog")
	mustContain(t, "overview", b, `href="/apps/blog/settings#terminal"`)
	_, b, _ = ts.viewer.get("/apps/blog/settings")
	mustNotContain(t, "viewer settings", b, "Terminal oturumları")

	if code, _, _ := ts.viewer.get(path); code != http.StatusForbidden {
		t.Errorf("viewer page: %d, want 403", code)
	}
	if code, _, _ := ts.other.get(path); code != http.StatusNotFound {
		t.Errorf("other team page: %d, want 404", code)
	}
	if code, _, _ := ts.owner.get("/apps/blog/deployments/99999/terminal"); code != http.StatusNotFound {
		t.Errorf("missing deployment: %d", code)
	}
}

func TestTerminalSessionAndAudit(t *testing.T) {
	ts := setupTerminal(t, true)
	path := termPath(ts.ready.ID)
	csrf := ts.owner.csrf(path)

	c, _, err := ts.dial(ts.owner, path+"?cols=100&rows=30", "self")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sendAuth(c, csrf)
	m := readJSON(t, c)
	if m["type"] != "ready" || m["pod"] != "d-0000000-abc" || m["process"] != "web" {
		t.Fatalf("ready: %v", m)
	}
	c.WriteMessage(wsock.BinaryMessage, []byte("uname"))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if op, data, err := c.ReadMessage(); err != nil || op != wsock.BinaryMessage || string(data) != "echo:uname" {
		t.Fatalf("echo: %d %q %v", op, data, err)
	}

	// The per-user limit (1 here) refuses a second session.
	c2, _, err := ts.dial(ts.owner, path, "self")
	if err != nil {
		t.Fatal(err)
	}
	sendAuth(c2, csrf)
	if m := readJSON(t, c2); m["type"] != "error" || !strings.Contains(m["message"].(string), "en fazla 1") {
		t.Fatalf("second session: %v", m)
	}
	expectCloseCode(t, c2, terminal.CodeTooMany)

	c.WriteMessage(wsock.BinaryMessage, []byte("exit"))
	if m := readJSON(t, c); m["type"] != "exit" || m["code"] != float64(0) {
		t.Fatalf("exit: %v", m)
	}
	expectCloseCode(t, c, wsock.CloseNormal)

	// The audit row: who, deployment, pod, exit code.
	var rows []store.AuditEntry
	for i := 0; i < 50; i++ {
		rows, _ = ts.st.ListAudit(context.Background(), ts.app.ID, store.AuditTerminal, 10)
		if len(rows) == 1 && rows[0].EndedAt != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rows) != 1 || rows[0].Actor != "alice" || rows[0].Pod != "d-0000000-abc" || rows[0].ExitCode == nil ||
		*rows[0].ExitCode != 0 || rows[0].EndReason != terminal.ReasonExit || *rows[0].DeploymentID != ts.ready.ID {
		t.Fatalf("audit rows %+v", rows)
	}
	_, b, _ := ts.owner.get("/apps/blog/settings")
	mustContain(t, "settings", b, "Terminal oturumları", "@alice", "d-0000000-abc", "çıkış kodu 0")

	// The worker pod can be chosen explicitly; a pod of another deployment cannot.
	c3, _, err := ts.dial(ts.owner, path+"?pod=d-0000000-w-worker-xyz", "self")
	if err != nil {
		t.Fatal(err)
	}
	sendAuth(c3, csrf)
	if m := readJSON(t, c3); m["type"] != "ready" || m["process"] != "worker" {
		t.Fatalf("worker session: %v", m)
	}
	c3.WriteClose(wsock.CloseNormal, "")
	expectCloseCode(t, c3, wsock.CloseNormal)
	time.Sleep(50 * time.Millisecond) // the slot is released after Run returns
	c4, _, err := ts.dial(ts.owner, path+"?pod=d-other-pod", "self")
	if err != nil {
		t.Fatal(err)
	}
	sendAuth(c4, csrf)
	if m := readJSON(t, c4); m["type"] != "error" {
		t.Fatalf("foreign pod: %v", m)
	}
	expectCloseCode(t, c4, terminal.CodeNoPod)
}

func TestTerminalSocketAuthorization(t *testing.T) {
	ts := setupTerminal(t, true)
	path := termPath(ts.ready.ID)
	csrf := ts.owner.csrf(path)

	status := func(c *ui, p, origin string) int {
		t.Helper()
		conn, resp, err := ts.dial(c, p, origin)
		if err == nil {
			conn.Close()
			return http.StatusSwitchingProtocols
		}
		if resp == nil {
			t.Fatalf("dial: %v", err)
		}
		return resp.StatusCode
	}
	for _, c := range []struct {
		name   string
		who    *ui
		path   string
		origin string
		want   int
	}{
		{"viewer", ts.viewer, path, "self", http.StatusForbidden},
		{"other team", ts.other, path, "self", http.StatusNotFound},
		{"foreign origin", ts.owner, path, "https://evil.example", http.StatusForbidden},
		{"no origin", ts.owner, path, "", http.StatusForbidden},
		{"bad csrf in query", ts.owner, path + "?csrf=nope", "self", http.StatusForbidden},
		{"good csrf in query", ts.owner, path + "?csrf=" + url.QueryEscape(csrf), "self", http.StatusSwitchingProtocols},
	} {
		if got := status(c.who, c.path, c.origin); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	// Not signed in: redirected to the login page, never upgraded.
	anon := &ui{t: t, st: ts.st, srv: ts.ui.srv, client: newClient()}
	if got := status(anon, path, "self"); got != http.StatusSeeOther {
		t.Errorf("anonymous: %d", got)
	}

	// A wrong token in the first message closes with 4003; nothing is exec'd.
	time.Sleep(50 * time.Millisecond)
	ts.cluster.mu.Lock()
	before := len(ts.cluster.execs)
	ts.cluster.mu.Unlock()
	c, _, err := ts.dial(ts.owner, path, "self")
	if err != nil {
		t.Fatal(err)
	}
	sendAuth(c, "wrong")
	if m := readJSON(t, c); m["type"] != "error" || m["reason"] != "csrf" {
		t.Fatalf("bad auth: %v", m)
	}
	expectCloseCode(t, c, terminal.CodeForbidden)
	// Keystrokes before auth are refused too.
	c, _, _ = ts.dial(ts.owner, path, "self")
	c.WriteMessage(wsock.BinaryMessage, []byte("id\r"))
	expectCloseCode(t, c, terminal.CodeForbidden)
	ts.cluster.mu.Lock()
	defer ts.cluster.mu.Unlock()
	if len(ts.cluster.execs) != before {
		t.Fatal("exec ran without a valid CSRF token")
	}
}

func TestTerminalDeploymentStates(t *testing.T) {
	ts := setupTerminal(t, true)
	ctx := context.Background()

	// Sleeping: the page offers to wake; the socket is refused.
	ts.st.SetSleeping(ctx, ts.ready.ID, true)
	path := termPath(ts.ready.ID)
	_, b, _ := ts.owner.get(path)
	mustContain(t, "sleeping", b, "uykuda", "Uyandır ve terminali aç")
	mustNotContain(t, "sleeping", b, "xterm.js")
	if _, resp, err := ts.dial(ts.owner, path, "self"); err == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("socket to a sleeping deployment: %v", err)
	}
	if code, _, _ := ts.viewer.post(fmt.Sprintf("/apps/blog/deployments/%d/wake", ts.ready.ID),
		url.Values{"csrf": {ts.viewer.csrf("/apps/blog")}}, "self"); code != http.StatusForbidden {
		t.Errorf("viewer wake: %d", code)
	}
	code, _, h := ts.owner.post(fmt.Sprintf("/apps/blog/deployments/%d/wake", ts.ready.ID),
		url.Values{"csrf": {ts.owner.csrf(path)}}, "self")
	if code != http.StatusSeeOther || h.Get("Location") != path || len(ts.waker.woken) != 1 {
		t.Fatalf("wake: %d %q %v", code, h.Get("Location"), ts.waker.woken)
	}
	_, b, _ = ts.owner.get(path)
	mustContain(t, "after wake", b, "xterm.js")

	// Retired: a message, no terminal, no button.
	d2, _, _ := ts.st.EnqueueDeployment(ctx, ts.app.ID, sha(2), "feature", "two")
	ts.st.MarkReady(ctx, d2, nil)
	if ok, err := ts.st.Retire(ctx, d2.ID, "test"); !ok || err != nil {
		t.Fatalf("retire: %v %v", ok, err)
	}
	_, b, _ = ts.owner.get(termPath(d2.ID))
	mustContain(t, "retired", b, "emekliye ayrıldı")
	mustNotContain(t, "retired", b, "xterm.js")
	if _, resp, err := ts.dial(ts.owner, termPath(d2.ID), "self"); err == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("socket to a retired deployment: %v", err)
	}
	_, b, _ = ts.owner.get("/apps/blog/deployments")
	mustNotContain(t, "deploys tab", b, termPath(d2.ID))

	// Queued: not running yet.
	d3, _, _ := ts.st.EnqueueDeployment(ctx, ts.app.ID, sha(3), "main", "three")
	_, b, _ = ts.owner.get(termPath(d3.ID))
	mustContain(t, "queued", b, "yalnızca hazır (çalışan) deploy")
}

func TestTerminalDisabled(t *testing.T) {
	ts := setupTerminal(t, false)
	path := termPath(ts.ready.ID)
	if code, _, _ := ts.owner.get(path); code != http.StatusNotFound {
		t.Errorf("page with the terminal off: %d", code)
	}
	if _, resp, err := ts.dial(ts.owner, path, "self"); err == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("socket with the terminal off: %v", err)
	}
	for _, p := range []string{"/apps/blog", "/apps/blog/deployments", fmt.Sprintf("/deployments/%d", ts.ready.ID)} {
		_, b, _ := ts.owner.get(p)
		mustNotContain(t, p, b, "/terminal", "Terminal oturumları")
	}
}
