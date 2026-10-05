package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "paas_testtoken"

var testNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// fakeAPI is an httptest server speaking the paas API.
type fakeAPI struct {
	*httptest.Server
	mux *http.ServeMux
	mu  sync.Mutex
	// bodies records request bodies by "METHOD path".
	bodies map[string][]string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{mux: http.NewServeMux(), bodies: map[string][]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"missing or invalid bearer token"}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		key := r.Method + " " + r.URL.Path
		f.bodies[key] = append(f.bodies[key], string(b))
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(b))
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	f.json("GET /api/me", 200, `{"login":"nisa","name":"Nisa","admin":false,"teams":[{"slug":"default","name":"Default","role":"owner"}]}`)
	return f
}

func (f *fakeAPI) json(pattern string, status int, body string) {
	f.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
}

func (f *fakeAPI) body(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies[key]...)
}

// isolate points os.UserConfigDir at a temp directory on every OS.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, env map[string]string, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	a := &App{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
		Getenv:         func(k string) string { return env[k] },
		Now:            func() time.Time { return testNow },
		HTTP:           &http.Client{Timeout: 10 * time.Second},
		OpenURL:        func(string) error { return nil },
		ReconnectDelay: 10 * time.Millisecond,
	}
	code := a.Run(context.Background(), args)
	return result{code, out.String(), errb.String()}
}

// loggedIn returns env vars that authenticate against f.
func loggedIn(f *fakeAPI) map[string]string {
	return map[string]string{"PAAS_URL": f.URL, "PAAS_TOKEN": testToken}
}

func TestLoginWritesConfig(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)

	res := run(t, nil, testToken+"\n", "login", "--url", f.URL)
	if res.code != ExitOK {
		t.Fatalf("login: code %d, stderr %q", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, f.URL+"/tokens") {
		t.Errorf("prompt should point at the tokens page: %q", res.stderr)
	}
	if !strings.Contains(res.stdout, "Logged in to "+f.URL+" as nisa (Nisa)") {
		t.Errorf("stdout %q", res.stdout)
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.URL != f.URL || cfg.Token != testToken {
		t.Fatalf("config %s = %s (%v)", path, b, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Errorf("config mode %v, want 0600", st.Mode().Perm())
		}
	}

	// The stored credentials are used by later commands.
	res = run(t, nil, "", "whoami")
	if res.code != ExitOK || !strings.Contains(res.stdout, "nisa (Nisa) on "+f.URL) || !strings.Contains(res.stdout, "default") {
		t.Fatalf("whoami: %+v", res)
	}
	// ... but never sent to another host given with --url.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("token leaked to another host: %s", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	if res = run(t, nil, "", "whoami", "--url", other.URL); res.code != ExitError || !strings.Contains(res.stderr, "not logged in") {
		t.Fatalf("whoami on another URL: %+v", res)
	}

	res = run(t, nil, "", "logout")
	if res.code != ExitOK || !strings.Contains(res.stdout, "Logged out") {
		t.Fatalf("logout: %+v", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config still exists: %v", err)
	}
	if res = run(t, nil, "", "whoami"); res.code != ExitError || !strings.Contains(res.stderr, "paas login") {
		t.Fatalf("whoami after logout: %+v", res)
	}
}

func TestLoginRejectsBadToken(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	res := run(t, nil, "", "login", "--url", f.URL, "--token", "paas_wrong")
	if res.code != ExitError {
		t.Fatalf("code %d", res.code)
	}
	if !strings.Contains(res.stderr, "401") || !strings.Contains(res.stderr, "Hint:") || !strings.Contains(res.stderr, "/tokens") {
		t.Errorf("stderr %q", res.stderr)
	}
	path, _ := ConfigPath()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a rejected token must not be saved")
	}
}

func TestLs(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.json("GET /api/apps", 200, `[
		{"id":1,"name":"blog","repo":"nisagwn/blog","production_branch":"main","production_url":"https://blog.example.com","created_at":"2026-01-01T00:00:00Z","team_id":1},
		{"id":2,"name":"shop","repo":"nisagwn/shop","production_branch":"main","production_url":"https://shop.example.com","created_at":"2026-01-01T00:00:00Z","team_id":1}]`)
	f.json("GET /api/apps/blog/deployments", 200, `[{"id":12,"app_name":"blog","commit_sha":"abcdef1234567","branch":"main","status":"ready","created_at":"2026-10-05T11:55:00Z","url":"https://abcdef1-blog.example.com"}]`)
	f.json("GET /api/apps/shop/deployments", 200, `[]`)

	res := run(t, loggedIn(f), "", "ls")
	if res.code != ExitOK {
		t.Fatalf("%+v", res)
	}
	for _, want := range []string{"NAME", "blog", "https://blog.example.com", "#12 abcdef1", "ready", "5m ago", "shop", "no deployments"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("ls output lacks %q:\n%s", want, res.stdout)
		}
	}

	res = run(t, loggedIn(f), "", "ls", "--json")
	var items []map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &items); err != nil || len(items) != 2 {
		t.Fatalf("ls --json: %v\n%s", err, res.stdout)
	}
	if ld, _ := items[0]["last_deployment"].(map[string]any); ld["id"] != float64(12) {
		t.Errorf("last_deployment %v", items[0]["last_deployment"])
	}
	if items[1]["last_deployment"] != nil {
		t.Errorf("shop last_deployment %v", items[1]["last_deployment"])
	}
}

func TestDeploymentsAndInspect(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.json("GET /api/apps/blog", 200, `{"name":"blog","production_url":"https://blog.example.com","aliases":[{"hostname":"blog.example.com","kind":"production","branch":"main","deployment_id":12}]}`)
	f.json("GET /api/apps/blog/deployments", 200, `[
		{"id":12,"commit_sha":"abcdef1234567","branch":"main","status":"ready","commit_message":"Add dark mode\n\nbody","created_at":"2026-10-05T09:00:00Z","started_at":"2026-10-05T09:00:00Z","finished_at":"2026-10-05T09:01:05Z"},
		{"id":11,"commit_sha":"1234567abcdef","branch":"main","status":"failed","created_at":"2026-10-03T12:00:00Z"}]`)
	f.json("GET /api/deployments/12", 200, `{"id":12,"app_name":"blog","commit_sha":"abcdef1234567","branch":"main","status":"ready","url":"https://abcdef1-blog.example.com","created_at":"2026-10-05T09:00:00Z","started_at":"2026-10-05T09:00:00Z","finished_at":"2026-10-05T09:01:05Z","sleeping":true}`)

	res := run(t, loggedIn(f), "", "deployments", "blog")
	for _, want := range []string{"production", "Add dark mode", "1m05s", "3h ago", "2d ago", "failed"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("deployments output lacks %q:\n%s", want, res.stdout)
		}
	}
	res = run(t, loggedIn(f), "", "inspect", "#12")
	for _, want := range []string{"#12", "ready (sleeping)", "https://abcdef1-blog.example.com", "1m05s"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("inspect output lacks %q:\n%s", want, res.stdout)
		}
	}
}

// sseHandler serves one scripted stream per connection and records ?after=.
func sseHandler(t *testing.T, scripts []string, afters *[]string) http.HandlerFunc {
	var mu sync.Mutex
	n := 0
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept %q", r.Header.Get("Accept"))
		}
		mu.Lock()
		i := n
		n++
		*afters = append(*afters, r.URL.Query().Get("after"))
		mu.Unlock()
		if i >= len(scripts) {
			http.Error(w, `{"error":"too many connections"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, scripts[i])
	}
}

func TestLogsFollowReconnectsUntilFinal(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	var afters []string
	f.mux.HandleFunc("GET /api/deployments/7/logs/stream", sseHandler(t, []string{
		// First connection drops before the deployment finished.
		"retry: 3000\n\n" +
			"event: status\ndata: {\"status\":\"building\",\"final\":false}\n\n" +
			"id: 1\ndata: #1 cloning\n\n" +
			": ping\n\n" +
			"id: 2\ndata: #2 building\n\n",
		// The reconnect resumes after line 2.
		"id: 3\ndata: pushed image\n\n" +
			"id: 4\ndata: ready at https://x\n\n" +
			"event: status\ndata: {\"status\":\"ready\",\"final\":true}\n\n" +
			"id: 5\ndata: never printed\n\n",
	}, &afters))

	res := run(t, loggedIn(f), "", "logs", "7", "-f")
	if res.code != ExitOK {
		t.Fatalf("%+v", res)
	}
	want := "#1 cloning\n#2 building\npushed image\nready at https://x\n"
	if res.stdout != want {
		t.Errorf("stdout %q, want %q", res.stdout, want)
	}
	if strings.Join(afters, ",") != "0,2" {
		t.Errorf("after params %v, want [0 2]", afters)
	}
	if !strings.Contains(res.stderr, "building") || !strings.Contains(res.stderr, "is ready") {
		t.Errorf("stderr %q", res.stderr)
	}
}

func TestLogsFollowFailedDeployment(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	var afters []string
	f.mux.HandleFunc("GET /api/deployments/8/logs/stream", sseHandler(t, []string{
		"id: 1\ndata: npm ERR!\n\nevent: status\ndata: {\"status\":\"failed\",\"error\":\"build failed\",\"final\":true}\n\n",
	}, &afters))
	res := run(t, loggedIn(f), "", "--json", "logs", "-f", "8")
	if res.code != ExitError || !strings.Contains(res.stderr, "deployment #8 failed: build failed") {
		t.Fatalf("%+v", res)
	}
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"line":"npm ERR!"`) || !strings.Contains(lines[1], `"final":true`) {
		t.Errorf("NDJSON output %q", res.stdout)
	}
}

func TestLogsPagesAndRuntime(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.mux.HandleFunc("GET /api/deployments/3/logs", func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after")
		var lines []logLine
		if after == "0" {
			for i := 1; i <= logPage; i++ {
				lines = append(lines, logLine{ID: int64(i), Line: fmt.Sprint("line ", i)})
			}
		} else if after == fmt.Sprint(logPage) {
			lines = []logLine{{ID: logPage + 1, Line: "last"}}
		}
		json.NewEncoder(w).Encode(lines)
	})
	f.mux.HandleFunc("GET /api/apps/blog/deployments/3/runtime-logs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("follow") != "1" || r.URL.Query().Get("tail") != "50" {
			t.Errorf("runtime query %s", r.URL.RawQuery)
		}
		io.WriteString(w, "listening on :8080\nGET / 200\n")
	})
	res := run(t, loggedIn(f), "", "logs", "3")
	if res.code != ExitOK || !strings.HasPrefix(res.stdout, "line 1\n") || !strings.HasSuffix(res.stdout, "line 1000\nlast\n") {
		t.Fatalf("code %d, stdout tail %q", res.code, res.stdout[max(0, len(res.stdout)-40):])
	}
	res = run(t, loggedIn(f), "", "logs", "--runtime", "blog", "3", "-f", "--tail", "50")
	if res.code != ExitOK || res.stdout != "listening on :8080\nGET / 200\n" {
		t.Fatalf("%+v", res)
	}
	if res = run(t, loggedIn(f), "", "logs", "blog", "3"); res.code != ExitUsage {
		t.Fatalf("two args without --runtime: %+v", res)
	}
}

func TestEnvSetRm(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.json("PUT /api/apps/blog/env", 200, `{"keys":["API_URL","DEBUG"]}`)
	f.json("GET /api/apps/blog/env", 200, `{"keys":["API_URL","DEBUG"]}`)

	res := run(t, loggedIn(f), "", "env", "set", "blog", "API_URL=https://a.example.com/?x=1", "DEBUG=")
	if res.code != ExitOK || !strings.Contains(res.stdout, "Set API_URL, DEBUG on blog") {
		t.Fatalf("%+v", res)
	}
	res = run(t, loggedIn(f), "", "env", "rm", "blog", "DEBUG")
	if res.code != ExitOK || !strings.Contains(res.stdout, "Removed DEBUG") {
		t.Fatalf("%+v", res)
	}
	bodies := f.body("PUT /api/apps/blog/env")
	if len(bodies) != 2 {
		t.Fatalf("PUT bodies %v", bodies)
	}
	var set, rm map[string]*string
	json.Unmarshal([]byte(bodies[0]), &set)
	json.Unmarshal([]byte(bodies[1]), &rm)
	if set["API_URL"] == nil || *set["API_URL"] != "https://a.example.com/?x=1" || set["DEBUG"] == nil || *set["DEBUG"] != "" {
		t.Errorf("set body %s", bodies[0])
	}
	if v, ok := rm["DEBUG"]; !ok || v != nil || len(rm) != 1 {
		t.Errorf("rm body %s, want {\"DEBUG\":null}", bodies[1])
	}

	res = run(t, loggedIn(f), "", "env", "ls", "blog")
	if res.code != ExitOK || res.stdout != "KEY\nAPI_URL\nDEBUG\n" {
		t.Fatalf("%+v", res)
	}
	for _, args := range [][]string{{"env", "set", "blog", "NOVALUE"}, {"env", "rm", "blog"}, {"env", "nope", "blog"}, {"env"}} {
		if res := run(t, loggedIn(f), "", args...); res.code != ExitUsage {
			t.Errorf("%v: code %d, want 2", args, res.code)
		}
	}
}

func TestRollback(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.json("POST /api/apps/blog/rollback", 200, `{"hostname":"blog.example.com","kind":"production","branch":"main","deployment_id":7}`)
	res := run(t, loggedIn(f), "", "rollback", "blog", "7")
	if res.code != ExitOK || !strings.Contains(res.stdout, "production (blog.example.com) now serves deployment #7") {
		t.Fatalf("%+v", res)
	}
	if b := f.body("POST /api/apps/blog/rollback"); len(b) != 1 || b[0] != `{"deployment_id":7}` {
		t.Errorf("body %v", b)
	}
	if res := run(t, loggedIn(f), "", "rollback", "blog", "abc"); res.code != ExitUsage {
		t.Errorf("bad id: %+v", res)
	}
}

func TestImportDomainsOpen(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.json("POST /api/apps/import", 201, `{"app":{"name":"web","production_branch":"main","production_url":"https://web.example.com"},"deployment":{"id":21,"commit_sha":"0123456789","status":"queued"}}`)
	f.json("POST /api/apps/web/domains", 201, `{"hostname":"www.example.org","status":"pending","dns_records":[{"type":"CNAME","name":"www.example.org","value":"web.example.com","note":"Recommended"}]}`)
	f.json("DELETE /api/apps/web/domains/www.example.org", 204, ``)
	f.json("GET /api/apps/web", 200, `{"name":"web","production_url":"https://web.example.com"}`)

	res := run(t, loggedIn(f), "", "import", "acme/web", "--team", "default")
	if res.code != ExitOK || !strings.Contains(res.stdout, "paas logs 21 -f") {
		t.Fatalf("%+v", res)
	}
	if b := f.body("POST /api/apps/import"); len(b) != 1 || b[0] != `{"repo":"acme/web","team":"default"}` {
		t.Errorf("import body %v", b)
	}
	res = run(t, loggedIn(f), "", "domains", "add", "web", "WWW.example.org")
	if res.code != ExitOK || !strings.Contains(res.stdout, "CNAME") || !strings.Contains(res.stdout, "pending") {
		t.Fatalf("%+v", res)
	}
	if res = run(t, loggedIn(f), "", "domains", "rm", "web", "www.example.org"); res.code != ExitOK {
		t.Fatalf("%+v", res)
	}
	res = run(t, loggedIn(f), "", "open", "web", "--print")
	if res.code != ExitOK || res.stdout != "https://web.example.com\n" {
		t.Fatalf("%+v", res)
	}
	if res = run(t, loggedIn(f), "", "import", "not-a-repo"); res.code != ExitUsage {
		t.Errorf("bad repo: %+v", res)
	}
}

func TestErrorMapping(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.json("GET /api/apps/secret/deployments", 404, `{"error":"app not found"}`)
	f.json("POST /api/apps/blog/rollback", 403, `{"error":"this action needs the member role on the team"}`)
	f.json("GET /api/deployments/9", 500, `{"error":"internal error"}`)

	cases := []struct {
		env  map[string]string
		args []string
		code int
		want []string
	}{
		{loggedIn(f), []string{"deployments", "secret"}, ExitError, []string{"app not found (HTTP 404)", "Hint:", "paas ls"}},
		{loggedIn(f), []string{"rollback", "blog", "3"}, ExitError, []string{"member role", "HTTP 403", "team owner"}},
		{map[string]string{"PAAS_URL": f.URL, "PAAS_TOKEN": "paas_revoked"}, []string{"ls"}, ExitError, []string{"HTTP 401", "/tokens"}},
		{loggedIn(f), []string{"inspect", "9"}, ExitError, []string{"HTTP 500", "try again"}},
		{nil, []string{"ls"}, ExitError, []string{"not logged in", "paas login"}},
		{map[string]string{"PAAS_URL": "http://127.0.0.1:1", "PAAS_TOKEN": testToken}, []string{"ls"}, ExitError, []string{"cannot reach", "Hint:"}},
		{loggedIn(f), []string{"frobnicate"}, ExitUsage, []string{"unknown command"}},
		{loggedIn(f), []string{"ls", "--bogus"}, ExitUsage, []string{"flag provided but not defined"}},
		{loggedIn(f), []string{"deployments"}, ExitUsage, []string{"missing arguments", "Usage: paas deployments"}},
		{loggedIn(f), []string{}, ExitUsage, []string{"Commands:"}},
	}
	for _, tc := range cases {
		res := run(t, tc.env, "", tc.args...)
		if res.code != tc.code {
			t.Errorf("%v: code %d, want %d (stderr %q)", tc.args, res.code, tc.code, res.stderr)
		}
		for _, w := range tc.want {
			if !strings.Contains(res.stderr, w) {
				t.Errorf("%v: stderr lacks %q:\n%s", tc.args, w, res.stderr)
			}
		}
	}
	if res := run(t, nil, "", "help"); res.code != ExitOK || !strings.Contains(res.stdout, "rollback") {
		t.Errorf("help: %+v", res)
	}
	if res := run(t, nil, "", "help", "logs"); res.code != ExitOK || !strings.Contains(res.stderr, "--runtime") {
		t.Errorf("help logs: %+v", res)
	}
}

func TestConfigHelpers(t *testing.T) {
	dir := isolate(t)
	for in, want := range map[string]string{
		"paas.example.com":          "https://paas.example.com",
		"http://localhost:8080/":    "http://localhost:8080",
		" https://h.example.com/x/": "https://h.example.com/x",
	} {
		if got, err := NormalizeURL(in); err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "ftp://x", "https://", "https://h/?a=1"} {
		if _, err := NormalizeURL(bad); err == nil {
			t.Errorf("NormalizeURL(%q) accepted", bad)
		}
	}
	path, err := ConfigPath()
	if err != nil || !strings.HasPrefix(path, dir) || filepath.Base(path) != "config.json" {
		t.Errorf("ConfigPath %q under %q: %v", path, dir, err)
	}
	if got := relTime(testNow, testNow.Add(-90*time.Second)); got != "1m ago" {
		t.Errorf("relTime %q", got)
	}
	if got := relTime(testNow, testNow.Add(-2*time.Second)); got != "just now" {
		t.Errorf("relTime %q", got)
	}
}
