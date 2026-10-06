package web_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/web"
)

// procCluster fakes the deployer's process side.
type procCluster struct{ runs []string }

func (c *procCluster) ProcessStatus(context.Context, store.Deployment) (process.Status, error) {
	return process.Status{
		Web:     &process.ReplicaStatus{Desired: 1, Ready: 1, State: "running"},
		Workers: map[string]process.ReplicaStatus{"queue": {Desired: 2, Ready: 2, State: "running"}},
		Crons:   map[string]process.CronStatus{"cleanup": {Exists: true}},
	}, nil
}

func (c *procCluster) RunCron(_ context.Context, _ store.Deployment, cron string) (string, error) {
	c.runs = append(c.runs, cron)
	return "job-1", nil
}

func (c *procCluster) ProcessLogs(context.Context, store.Deployment, string, bool, int64, io.Writer) error {
	return nil
}

type syncCounter struct{ n int }

func (s *syncCounter) SyncApp(context.Context, string) error { s.n++; return nil }

func TestProcessesSection(t *testing.T) {
	st := testdb.Open(t)
	cluster, router := &procCluster{}, &syncCounter{}
	s := &web.Server{
		Store: st, Sessions: auth.New(token), Domain: "paas.test", Router: router, Processes: cluster,
		RuntimeLogs: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	u := &ui{t: t, st: st, srv: srv, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
	u.login()
	ctx := context.Background()
	u.post("/apps", url.Values{"name": {"blog"}, "repo": {"nisagwn/blog"}, "csrf": {u.csrf("/")}}, "self")
	app, _ := st.GetAppByName(ctx, "blog")

	if code, body, _ := u.get("/apps/blog/processes"); code != 200 || !strings.Contains(body, "ilk canlı deploy") {
		t.Fatalf("before production: %d\n%s", code, body)
	}

	d, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "")
	st.SetProcesses(ctx, d.ID, process.Set{
		Source:  "paas.yaml",
		Workers: []process.Worker{{Name: "queue", Command: "node queue.js", Replicas: 2}},
		Crons:   []process.Cron{{Name: "cleanup", Schedule: "*/15 * * * *", Command: "node cleanup.js"}},
	})
	st.MarkReady(ctx, d, []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}})

	if _, body, _ := u.get("/apps/blog"); !strings.Contains(body, `hx-get="/apps/blog/processes"`) || !strings.Contains(body, "Süreçler") {
		t.Fatalf("app page has no processes section:\n%s", body)
	}
	code, body, _ := u.get("/apps/blog/processes")
	for _, want := range []string{"queue", "Arka plan", "node queue.js", "2/2", "Çalışıyor", "cleanup", "*/15 * * * *",
		"Şimdi çalıştır", "Ölçekle", "paas.yaml", "?process=queue"} {
		if !strings.Contains(body, want) {
			t.Errorf("processes partial lacks %q", want)
		}
	}
	if code != 200 || t.Failed() {
		t.Fatalf("%d\n%s", code, body)
	}

	csrf := u.csrf("/apps/blog/processes")
	htmx := func(path string, form url.Values) (int, string) {
		form.Set("csrf", csrf)
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", srv.URL)
		req.Header.Set("HX-Request", "true")
		resp, err := u.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := htmx("/apps/blog/processes/scale", url.Values{"process": {"queue"}, "replicas": {"3"}}); code != 200 ||
		!strings.Contains(body, "queue için kopya sayısı 3 yapıldı") {
		t.Fatalf("scale: %d\n%s", code, body)
	}
	if got, _ := st.ProcessReplicas(ctx, app.ID); got["queue"] != 3 || router.n != 1 {
		t.Errorf("override = %v, syncs = %d", got, router.n)
	}
	if code, body := htmx("/apps/blog/processes/scale", url.Values{"process": {"web"}, "replicas": {"2"}}); code != 400 ||
		!strings.Contains(body, "Web süreci elle ölçeklenmez") {
		t.Errorf("scale web: %d\n%s", code, body)
	}
	if code, body := htmx("/apps/blog/processes/scale", url.Values{"process": {"queue"}, "replicas": {"x"}}); code != 400 ||
		!strings.Contains(body, "0 ile 10") {
		t.Errorf("bad replicas: %d\n%s", code, body)
	}
	if code, body := htmx("/apps/blog/crons/run", url.Values{"cron": {"cleanup"}}); code != 202 ||
		!strings.Contains(body, "cleanup başlatıldı") || len(cluster.runs) != 1 {
		t.Errorf("run: %d %v\n%s", code, cluster.runs, body)
	}
	if code, body := htmx("/apps/blog/crons/run", url.Values{"cron": {"nope"}}); code != 404 ||
		!strings.Contains(body, "nope&#34; adlı bir zamanlanmış iş yok") {
		t.Errorf("unknown cron: %d\n%s", code, body)
	}

	// The deployment page offers the processes in its runtime log.
	if _, body, _ := u.get(fmt.Sprintf("/deployments/%d", d.ID)); !strings.Contains(body, `id="rt-process"`) ||
		!strings.Contains(body, `<option value="queue">`) || !strings.Contains(body, `<option value="cleanup">`) {
		t.Errorf("deployment page lacks the process selector")
	}
}
