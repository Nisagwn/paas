package scale

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const (
	shaPrev = "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaProd = "2222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeCluster keeps workloads in memory; Sleep and Wake flip their state.
type fakeCluster struct {
	mu        sync.Mutex
	ws        map[string]*deploy.Workload
	sleeps    int
	wakes     atomic.Int32
	wakeDelay time.Duration
	wakeErr   error
	podAddr   string
	hosts     map[string]string // host → workload key
}

func newCluster() *fakeCluster {
	c := &fakeCluster{ws: map[string]*deploy.Workload{}, hosts: map[string]string{}}
	for id, sha := range map[int64]string{1: shaPrev, 2: shaProd} {
		w := &deploy.Workload{Namespace: "app-blog", Name: "d-" + sha[:7], DeploymentID: id, Replicas: 1, Available: 1}
		w.MetricKey = deploy.MetricKey(w.Namespace, w.Name)
		c.ws[w.Key()] = w
	}
	c.hosts["1111111-blog.paas.test"] = "app-blog/d-1111111"
	return c
}

func (c *fakeCluster) get(key string) deploy.Workload {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.ws[key]
}

func (c *fakeCluster) Workloads(context.Context) ([]deploy.Workload, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []deploy.Workload
	for _, w := range c.ws {
		out = append(out, *w)
	}
	return out, nil
}

func (c *fakeCluster) Sleep(_ context.Context, ns, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.ws[ns+"/"+name]
	w.Sleeping, w.Replicas, w.Available = true, 0, 0
	c.sleeps++
	return nil
}

func (c *fakeCluster) Wake(_ context.Context, ns, name string) error {
	c.wakes.Add(1)
	time.Sleep(c.wakeDelay)
	if c.wakeErr != nil {
		return c.wakeErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.ws[ns+"/"+name]
	w.Sleeping, w.Replicas, w.Available = false, 1, 1
	return nil
}

func (c *fakeCluster) RepointActivator(context.Context) error { return nil }

func (c *fakeCluster) Backend(_ context.Context, host string) (deploy.Workload, error) {
	key, ok := c.hosts[host]
	if !ok {
		return deploy.Workload{}, deploy.ErrNotFound
	}
	return c.get(key), nil
}

func (c *fakeCluster) PodAddr(context.Context, string, string) (string, error) {
	if c.podAddr == "" {
		return "", errors.New("no ready pod")
	}
	return c.podAddr, nil
}

type fakeCounter struct {
	mu     sync.Mutex
	counts map[string]float64
	err    error
}

func (f *fakeCounter) RequestCounts(context.Context) (map[string]float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]float64{}
	for k, v := range f.counts {
		out[k] = v
	}
	return out, f.err
}

func (f *fakeCounter) set(key string, v float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[key] = v
}

type fakeStore struct {
	mu       sync.Mutex
	cands    []store.ScaleCandidate
	sleeping map[int64]bool
}

func (f *fakeStore) ScaleCandidates(context.Context) ([]store.ScaleCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]store.ScaleCandidate(nil), f.cands...)
	for i := range out {
		if f.sleeping[out[i].DeploymentID] {
			out[i].SleepingSince = &time.Time{}
		}
	}
	return out, nil
}

func (f *fakeStore) SetSleeping(_ context.Context, id int64, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleeping[id] = on
	return nil
}

func (f *fakeStore) isSleeping(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sleeping[id]
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	s       *Scaler
	cluster *fakeCluster
	counter *fakeCounter
	store   *fakeStore
	clock   *clock
}

const (
	prevKey = "app-blog/d-1111111"
	prodKey = "app-blog/d-2222222"
)

func newEnv(t *testing.T, scaleProduction bool) *env {
	t.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := &env{
		cluster: newCluster(),
		counter: &fakeCounter{counts: map[string]float64{}},
		store: &fakeStore{sleeping: map[int64]bool{}, cands: []store.ScaleCandidate{
			{DeploymentID: 1, AppName: "blog", CommitSHA: shaPrev, FinishedAt: start.Add(-time.Hour)},
			{DeploymentID: 2, AppName: "blog", CommitSHA: shaProd, FinishedAt: start.Add(-time.Hour),
				Production: true, ScaleProduction: scaleProduction},
		}},
		clock: &clock{t: start},
	}
	e.s = &Scaler{Cluster: e.cluster, Counter: e.counter, Store: e.store, IdleAfter: 10 * time.Minute, Log: quiet,
		now: e.clock.now}
	return e
}

// waitFor polls cond: proactive wakes run in the background.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestIdlePreviewSleeps(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()

	// First check: baseline only (history before start is unknown).
	if r := e.s.Check(ctx); r.Slept != 0 {
		t.Fatalf("slept at start: %+v", r)
	}
	e.clock.advance(6 * time.Minute)
	e.counter.set(deploy.MetricKey("app-blog", "d-1111111"), 3) // traffic
	if r := e.s.Check(ctx); r.Slept != 0 {
		t.Fatalf("slept while active: %+v", r)
	}
	e.clock.advance(9 * time.Minute) // 15m since start, 9m since traffic
	if r := e.s.Check(ctx); r.Slept != 0 {
		t.Fatalf("slept too early: %+v", r)
	}
	e.clock.advance(time.Minute)
	if r := e.s.Check(ctx); r.Slept != 1 {
		t.Fatalf("want one sleep: %+v", r)
	}
	if !e.cluster.get(prevKey).Sleeping || !e.store.isSleeping(1) {
		t.Fatal("preview not asleep")
	}
	// Production (no opt-in) stays awake however idle it is.
	if e.cluster.get(prodKey).Sleeping {
		t.Fatal("production slept without opt-in")
	}
	// Nothing more to do on the next check.
	if r := e.s.Check(ctx); r.Slept != 0 || r.Woken != 0 {
		t.Fatalf("second check: %+v", r)
	}
}

func TestProductionOptIn(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	e.s.Check(ctx)
	e.clock.advance(11 * time.Minute)
	if r := e.s.Check(ctx); r.Slept != 2 {
		t.Fatalf("want both asleep: %+v", r)
	}

	// Opt-in withdrawn: the scaler wakes production on its next check.
	e.store.mu.Lock()
	e.store.cands[1].ScaleProduction = false
	e.store.mu.Unlock()
	if r := e.s.Check(ctx); r.Woken != 1 {
		t.Fatalf("want production woken: %+v", r)
	}
	waitFor(t, "production awake", func() bool { return !e.cluster.get(prodKey).Sleeping && !e.store.isSleeping(2) })
	if !e.cluster.get(prevKey).Sleeping {
		t.Fatal("preview woke up too")
	}
}

func TestCounterErrorKeepsAwake(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	e.s.Check(ctx)
	e.clock.advance(time.Hour)
	e.counter.err = errors.New("traefik unreachable")
	if r := e.s.Check(ctx); r.Slept != 0 {
		t.Fatalf("slept without counters: %+v", r)
	}
}

func TestCounterResetIsActivity(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	key := deploy.MetricKey("app-blog", "d-1111111")
	e.counter.set(key, 500)
	e.s.Check(ctx)
	e.clock.advance(9 * time.Minute)
	e.counter.set(key, 2) // Traefik restarted and served two requests
	e.s.Check(ctx)
	e.clock.advance(9 * time.Minute)
	if r := e.s.Check(ctx); r.Slept != 0 {
		t.Fatalf("reset not counted as activity: %+v", r)
	}
}

func TestDisabledNeverSleeps(t *testing.T) {
	e := newEnv(t, true)
	e.s.IdleAfter = 0
	e.s.Check(context.Background())
	e.clock.advance(24 * time.Hour)
	if r := e.s.Check(context.Background()); r.Slept != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestStateMirroredToStore(t *testing.T) {
	e := newEnv(t, false)
	e.cluster.Sleep(context.Background(), "app-blog", "d-1111111") // e.g. slept before a restart
	e.s.Check(context.Background())
	if !e.store.isSleeping(1) {
		t.Fatal("sleeping state not recorded")
	}
}

func TestInterruptedWakeIsFinished(t *testing.T) {
	e := newEnv(t, false)
	e.cluster.Sleep(context.Background(), "app-blog", "d-1111111")
	e.cluster.mu.Lock()
	e.cluster.ws[prevKey].Replicas = 1 // scaled up, but never switched back
	e.cluster.mu.Unlock()
	if r := e.s.Check(context.Background()); r.Woken != 1 {
		t.Fatalf("%+v", r)
	}
	waitFor(t, "wake", func() bool { return !e.cluster.get(prevKey).Sleeping })
}

func TestConcurrentWakesShareOne(t *testing.T) {
	e := newEnv(t, false)
	e.cluster.Sleep(context.Background(), "app-blog", "d-1111111")
	e.cluster.wakeDelay = 50 * time.Millisecond
	w := e.cluster.get(prevKey)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.s.Wake(context.Background(), w); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := e.cluster.wakes.Load(); n != 1 {
		t.Fatalf("Cluster.Wake called %d times", n)
	}
	if e.cluster.get(prevKey).Sleeping || e.store.isSleeping(1) {
		t.Fatal("still asleep")
	}
}

func TestWakeResetsIdleClock(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	e.s.Check(ctx)
	e.clock.advance(11 * time.Minute)
	e.s.Check(ctx) // sleeps the preview
	if err := e.s.Wake(ctx, e.cluster.get(prevKey)); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(5 * time.Minute)
	if r := e.s.Check(ctx); r.Slept != 0 {
		t.Fatalf("slept again right after waking: %+v", r)
	}
	e.clock.advance(6 * time.Minute)
	if r := e.s.Check(ctx); r.Slept != 1 {
		t.Fatalf("want sleep after another idle period: %+v", r)
	}
}

// A deployment asleep since before a restart is woken by a request: its
// idle time counts from the wake, not from the scaler's start.
func TestWakeBeforeFirstObservation(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	e.cluster.Sleep(ctx, "app-blog", "d-1111111")
	e.s.Check(ctx)
	e.clock.advance(9 * time.Minute)
	if err := e.s.Wake(ctx, e.cluster.get(prevKey)); err != nil {
		t.Fatal(err)
	}
	e.counter.set(deploy.MetricKey("app-blog", "d-1111111"), 7)
	e.s.Check(ctx)
	e.clock.advance(2 * time.Minute) // 11m after start, 2m after the wake
	if r := e.s.Check(ctx); r.Slept != 0 {
		t.Fatalf("slept 2m after waking: %+v", r)
	}
	e.clock.advance(8 * time.Minute)
	if r := e.s.Check(ctx); r.Slept != 1 {
		t.Fatalf("want sleep 10m after waking: %+v", r)
	}
}

// ---- activator ----

func newActivator(t *testing.T, e *env, upstream string) *httptest.Server {
	t.Helper()
	a := &Activator{Scaler: e.s, Cluster: e.cluster, Upstream: upstream, Timeout: 2 * time.Second, Log: quiet}
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return srv
}

func request(t *testing.T, srv *httptest.Server, host, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hello?x=1", strings.NewReader(body))
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestActivatorWakesAndProxiesToPod(t *testing.T) {
	e := newEnv(t, false)
	e.cluster.Sleep(context.Background(), "app-blog", "d-1111111")
	e.cluster.wakeDelay = 100 * time.Millisecond
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		io.WriteString(w, r.Method+" "+r.Host+" "+r.URL.RequestURI()+" "+string(b))
	}))
	defer app.Close()
	e.cluster.podAddr = strings.TrimPrefix(app.URL, "http://")
	srv := newActivator(t, e, "pod")

	resp, body := request(t, srv, "1111111-blog.paas.test", "payload")
	if resp.StatusCode != 200 || body != "POST 1111111-blog.paas.test /hello?x=1 payload" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if e.cluster.get(prevKey).Sleeping || e.store.isSleeping(1) {
		t.Fatal("not woken")
	}
}

func TestActivatorIngressUpstreamRetriesLoop(t *testing.T) {
	e := newEnv(t, false)
	e.cluster.Sleep(context.Background(), "app-blog", "d-1111111")
	var hops atomic.Int32
	// The ingress controller still routes the first two re-sent requests
	// back to the activator, then reaches the pod.
	var act http.Handler
	ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HopHeader) == "" {
			t.Error("re-sent request without hop header")
		}
		if hops.Add(1) <= 2 {
			act.ServeHTTP(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		io.WriteString(w, "app saw "+r.Host+" "+string(b))
	}))
	defer ingress.Close()
	a := &Activator{Scaler: e.s, Cluster: e.cluster, Upstream: ingress.URL, Timeout: 2 * time.Second, Log: quiet}
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	act = a
	srv := httptest.NewServer(a)
	defer srv.Close()

	resp, body := request(t, srv, "1111111-blog.paas.test", "abc")
	if resp.StatusCode != 200 || body != "app saw 1111111-blog.paas.test abc" || hops.Load() != 3 {
		t.Fatalf("%d %q hops=%d", resp.StatusCode, body, hops.Load())
	}
}

func TestActivatorErrors(t *testing.T) {
	e := newEnv(t, false)
	srv := newActivator(t, e, "pod")

	if resp, _ := request(t, srv, "unknown.paas.test", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown host: %d", resp.StatusCode)
	}

	e.cluster.Sleep(context.Background(), "app-blog", "d-1111111")
	e.cluster.wakeErr = errors.New("pod CrashLoopBackOff")
	resp, _ := request(t, srv, "1111111-blog.paas.test", "")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("failed wake: %d", resp.StatusCode)
	}

	// A request carrying the hop header is answered as a loop at once.
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Host = "1111111-blog.paas.test"
	req.Header.Set(HopHeader, "1")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusServiceUnavailable || r2.Header.Get(LoopHeader) == "" {
		t.Fatalf("loop: %d", r2.StatusCode)
	}
}

func TestActivatorUpstreamValidation(t *testing.T) {
	for _, u := range []string{"ftp://x", "not a url", "http://"} {
		if err := (&Activator{Upstream: u}).Init(); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}
