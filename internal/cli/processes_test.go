package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

const psBody = `{"deployment_id":12,"production":true,"source":"paas.yaml",
 "processes":[
  {"name":"web","type":"web","command":"","replicas":1,"desired":1,"ready":1,"state":"running"},
  {"name":"queue","type":"worker","command":"node worker.js","replicas":1,"override":3,"desired":3,"ready":2,"state":"starting"},
  {"name":"bot","type":"worker","command":"python bot.py","replicas":1,"desired":1,"ready":0,"state":"crashing","reason":"CrashLoopBackOff"}],
 "crons":[
  {"name":"cleanup","schedule":"*/15 * * * *","command":"node cleanup.js","exists":true,"suspended":false,
   "last_job":{"name":"j","status":"succeeded","manual":true,"started":"2026-10-05T11:55:00Z"}},
  {"name":"report","schedule":"@daily","command":"node report.js","exists":true,"suspended":true}]}`

func TestPs(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.json("GET /api/apps/blog/processes", 200, psBody)
	res := run(t, loggedIn(f), "", "ps", "blog")
	if res.code != ExitOK {
		t.Fatalf("code %d: %s", res.code, res.stderr)
	}
	for _, want := range []string{
		"Deployment #12 (production) · paas.yaml",
		"web      web     1/1",
		"(detected start command)",
		"queue    worker  2/3 (set: 3, paas.yaml: 1)  starting",
		"crashing (CrashLoopBackOff)",
		"cleanup  */15 * * * *    5m ago (manual)",
		"succeeded",
		"report   @daily",
		"suspended",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}

	var gotQuery string
	f.mux.HandleFunc("GET /api/apps/shop/processes", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		io.WriteString(w, `{"deployment_id":7,"processes":[],"crons":[]}`)
	})
	if res := run(t, loggedIn(f), "", "ps", "shop", "--deployment", "7"); res.code != ExitOK || gotQuery != "deployment=7" ||
		!strings.Contains(res.stdout, "not production") {
		t.Errorf("--deployment: %d %q %q", res.code, gotQuery, res.stdout)
	}
	if res := run(t, loggedIn(f), "", "ps", "blog", "--json"); res.code != ExitOK || !strings.Contains(res.stdout, `"deployment_id": 12`) {
		t.Errorf("--json: %q", res.stdout)
	}
}

func TestPsScaleAndCronRun(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.json("PUT /api/apps/blog/processes/queue", 200, psBody)
	f.json("PUT /api/apps/blog/processes/bot", 200, psBody)
	res := run(t, loggedIn(f), "", "ps", "scale", "blog", "queue=3", "bot=default")
	if res.code != ExitOK {
		t.Fatalf("code %d: %s", res.code, res.stderr)
	}
	if got := f.body("PUT /api/apps/blog/processes/queue"); len(got) != 1 || got[0] != `{"replicas":3}` {
		t.Errorf("queue body = %q", got)
	}
	if got := f.body("PUT /api/apps/blog/processes/bot"); len(got) != 1 || got[0] != `{"replicas":null}` {
		t.Errorf("bot body = %q", got)
	}
	if !strings.Contains(res.stdout, "Scaled queue to 3 replica(s).") || !strings.Contains(res.stdout, "Scaled bot back to its paas.yaml replicas.") {
		t.Errorf("stdout %q", res.stdout)
	}
	for _, args := range [][]string{
		{"ps", "scale", "blog"},
		{"ps", "scale", "blog", "queue"},
		{"ps", "scale", "blog", "queue=11"},
		{"ps", "scale", "blog", "queue=x"},
		{"ps", "blog", "extra"},
		{"cron", "start", "blog", "cleanup"},
		{"cron", "run", "blog"},
		{"logs", "--process", "queue", "12"},
	} {
		if res := run(t, loggedIn(f), "", args...); res.code != ExitUsage {
			t.Errorf("%v: code %d, want usage error", args, res.code)
		}
	}

	f.json("POST /api/apps/blog/crons/cleanup/run", 202, `{"cron":"cleanup","job":"d-abc-c-cleanup-m-1"}`)
	res = run(t, loggedIn(f), "", "cron", "run", "blog", "cleanup")
	if res.code != ExitOK || !strings.Contains(res.stdout, "Started cleanup of blog (job d-abc-c-cleanup-m-1)") {
		t.Errorf("cron run: %d %q %q", res.code, res.stdout, res.stderr)
	}
	f.json("POST /api/apps/blog/crons/busy/run", 409, `{"error":"a run of this cron job is still active"}`)
	if res := run(t, loggedIn(f), "", "cron", "run", "blog", "busy"); res.code != ExitError ||
		!strings.Contains(res.stderr, "still active") {
		t.Errorf("busy cron: %d %q", res.code, res.stderr)
	}
}

func TestRuntimeLogsProcess(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	var gotQuery string
	f.mux.HandleFunc("GET /api/apps/blog/deployments/12/runtime-logs", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		io.WriteString(w, "worker line\n")
	})
	res := run(t, loggedIn(f), "", "logs", "--runtime", "--process", "queue", "blog", "12")
	if res.code != ExitOK || res.stdout != "worker line\n" || gotQuery != "tail=200&process=queue" {
		t.Fatalf("%d %q %q", res.code, res.stdout, gotQuery)
	}
}
