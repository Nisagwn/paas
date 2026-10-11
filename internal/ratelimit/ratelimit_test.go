package ratelimit_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/ratelimit"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestParseRule(t *testing.T) {
	for in, want := range map[string]ratelimit.Rule{
		"10/1m": {Rate: 10, Per: time.Minute}, " 100 / 1h ": {Rate: 100, Per: time.Hour},
		"off": {}, "OFF": {}, "0": {}, "": {},
	} {
		got, err := ratelimit.ParseRule(in)
		if err != nil || got != want {
			t.Errorf("ParseRule(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"10", "x/1m", "10/x", "10/-1m", "-1/1m", "10/0s"} {
		if _, err := ratelimit.ParseRule(bad); err == nil {
			t.Errorf("ParseRule(%q) accepted", bad)
		}
	}
	if s := (ratelimit.Rule{Rate: 5, Per: time.Minute}).String(); s != "5/1m0s" {
		t.Errorf("String %q", s)
	}
	if s := (ratelimit.Rule{}).String(); s != "off" {
		t.Errorf("String of off %q", s)
	}
}

// The bucket holds Rate tokens and refills one per Per/Rate.
func TestTokenBucket(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := ratelimit.New(ratelimit.Rule{Rate: 2, Per: time.Minute}, c.now)
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("token %d refused", i)
		}
	}
	ok, wait := l.Allow("k")
	if ok || wait != 30*time.Second {
		t.Fatalf("empty bucket: ok=%v wait=%s", ok, wait)
	}
	// Other keys have their own buckets.
	if ok, _ := l.Allow("other"); !ok {
		t.Fatal("other key refused")
	}
	c.add(29 * time.Second)
	if ok, wait := l.Allow("k"); ok || wait <= 0 || wait > time.Second {
		t.Fatalf("before refill: ok=%v wait=%s", ok, wait)
	}
	c.add(time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("after refill refused")
	}
	// A long pause refills at most Rate tokens.
	c.add(time.Hour)
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("after pause, token %d refused", i)
		}
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("bucket holds more than Rate")
	}
}

func TestDisabledAndNil(t *testing.T) {
	var nilLimiter *ratelimit.Limiter
	if ok, _ := nilLimiter.Allow("k"); !ok {
		t.Fatal("nil limiter refused")
	}
	off := ratelimit.New(ratelimit.Rule{}, nil)
	for i := 0; i < 1000; i++ {
		if ok, _ := off.Allow("k"); !ok {
			t.Fatal("disabled limiter refused")
		}
	}
	var set *ratelimit.Set
	if ok, _ := set.Allow(off, httptest.NewRequest("GET", "/", nil), "u"); !ok {
		t.Fatal("nil set refused")
	}
}

// Full buckets are swept, so the map does not grow without bound.
func TestSweep(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := ratelimit.New(ratelimit.Rule{Rate: 1, Per: time.Second}, c.now)
	for i := 0; i < 1023; i++ {
		l.Allow(string(rune('a'+i%26)) + time.Duration(i).String())
	}
	if l.Len() != 1023 {
		t.Fatalf("len %d", l.Len())
	}
	c.add(2 * time.Second)
	l.Allow("trigger") // the 1024th call sweeps
	if l.Len() != 1 {
		t.Fatalf("after sweep len %d", l.Len())
	}
}

// A Set limits per IP and per user; X-Forwarded-For counts only when
// trusted.
func TestSetPerIPAndUser(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := ratelimit.New(ratelimit.Rule{Rate: 2, Per: time.Hour}, c.now)
	set := &ratelimit.Set{Writes: l}
	req := func(ip, xff string) *http.Request {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = ip + ":1234"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	if ip := set.ClientIP(req("10.0.0.1", "1.2.3.4")); ip != "10.0.0.1" {
		t.Fatalf("untrusted XFF used: %s", ip)
	}
	// The same user from two addresses: the user bucket runs out.
	for i, ip := range []string{"10.0.0.1", "10.0.0.2"} {
		if ok, _ := set.Allow(l, req(ip, ""), "42"); !ok {
			t.Fatalf("request %d refused", i)
		}
	}
	if ok, _ := set.Allow(l, req("10.0.0.3", ""), "42"); ok {
		t.Fatal("user bucket not applied")
	}
	// Anonymous requests from one address: the IP bucket runs out.
	if ok, _ := set.Allow(l, req("10.0.0.9", ""), ""); !ok {
		t.Fatal("first anonymous request refused")
	}
	set.Allow(l, req("10.0.0.9", ""), "")
	if ok, _ := set.Allow(l, req("10.0.0.9", ""), "7"); ok {
		t.Fatal("IP bucket not applied")
	}

	trusted := &ratelimit.Set{Writes: l, TrustForwarded: true}
	if ip := trusted.ClientIP(req("10.0.0.1", "9.9.9.9, 1.2.3.4")); ip != "1.2.3.4" {
		t.Fatalf("trusted XFF: %s", ip)
	}
	if ip := trusted.ClientIP(req("10.0.0.1", "")); ip != "10.0.0.1" {
		t.Fatalf("no XFF: %s", ip)
	}
}

func TestRetryAfter(t *testing.T) {
	for wait, want := range map[time.Duration]string{0: "1", 300 * time.Millisecond: "1", 1500 * time.Millisecond: "2", time.Minute: "60"} {
		rec := httptest.NewRecorder()
		ratelimit.RetryAfter(rec, wait)
		if got := rec.Header().Get("Retry-After"); got != want {
			t.Errorf("RetryAfter(%s) = %s, want %s", wait, got, want)
		}
	}
}
