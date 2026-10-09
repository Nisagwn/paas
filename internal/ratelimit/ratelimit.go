// Package ratelimit is an in-memory token bucket limiter (Faz 23): sign-in,
// imports and API writes are limited per client IP and per user, so one
// account or address cannot flood a public installation.
//
// Buckets live in the process: with several control-plane replicas each
// enforces its own share. That bounds abuse, which is the goal; it is not
// a billing-grade quota (plans are, see internal/plans).
package ratelimit

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rule allows Rate events per Per, in bursts of up to Rate. A zero Rule
// allows everything.
type Rule struct {
	Rate int
	Per  time.Duration
}

// Off reports whether the rule allows everything.
func (r Rule) Off() bool { return r.Rate <= 0 || r.Per <= 0 }

func (r Rule) String() string {
	if r.Off() {
		return "off"
	}
	return fmt.Sprintf("%d/%s", r.Rate, r.Per)
}

// ParseRule reads "<rate>/<duration>" ("10/1m", "100/1h"); "0", "off" and
// "" disable the limit.
func ParseRule(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" || strings.EqualFold(s, "off") {
		return Rule{}, nil
	}
	rate, per, ok := strings.Cut(s, "/")
	n, err := strconv.Atoi(strings.TrimSpace(rate))
	if !ok || err != nil || n < 0 {
		return Rule{}, errors.New(`rate limit must look like "10/1m" (events per duration) or "off"`)
	}
	d, err := time.ParseDuration(strings.TrimSpace(per))
	if err != nil || d <= 0 {
		return Rule{}, errors.New(`rate limit must look like "10/1m" (events per duration) or "off"`)
	}
	return Rule{Rate: n, Per: d}, nil
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter keeps one bucket per key. The zero value is not usable; use New.
type Limiter struct {
	rule Rule
	now  func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	calls   int
}

// New returns a limiter for rule; now nil is time.Now.
func New(rule Rule, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{rule: rule, now: now, buckets: map[string]*bucket{}}
}

// Rule returns the limiter's rule.
func (l *Limiter) Rule() Rule {
	if l == nil {
		return Rule{}
	}
	return l.rule
}

// sweepEvery is how many calls pass between removals of full buckets.
const sweepEvery = 1024

// Allow takes a token for key. It returns false and the wait until the
// next token when the bucket is empty. A nil or disabled limiter allows.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l == nil || l.rule.Off() {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.calls++
	if l.calls%sweepEvery == 0 {
		l.sweep(now)
	}
	perToken := l.rule.Per / time.Duration(l.rule.Rate)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.rule.Rate), last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += float64(elapsed) / float64(perToken)
		if b.tokens > float64(l.rule.Rate) {
			b.tokens = float64(l.rule.Rate)
		}
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) * float64(perToken))
	return false, wait
}

// sweep drops buckets that refilled completely: they behave like new ones.
func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) >= l.rule.Per {
			delete(l.buckets, k)
		}
	}
}

// Len is the number of tracked keys (tests).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Set is the limiters of the control plane.
type Set struct {
	// SignIn: GitHub sign-in and the token login, per IP.
	SignIn *Limiter
	// Import: creating and importing apps, per user and per IP.
	Import *Limiter
	// Writes: every state-changing API request and web form, per user and
	// per IP.
	Writes *Limiter
	// TrustForwarded takes the client address from the last
	// X-Forwarded-For entry (set by the ingress controller in front of
	// the control plane) instead of the TCP peer.
	TrustForwarded bool
}

// ClientIP is the address requests are limited by.
func (s *Set) ClientIP(r *http.Request) string {
	if s != nil && s.TrustForwarded {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Allow takes a token from l for the request's IP and for user ("" skips
// the user bucket). It returns false and the wait when either is empty.
func (s *Set) Allow(l *Limiter, r *http.Request, user string) (bool, time.Duration) {
	if s == nil || l == nil {
		return true, 0
	}
	if ok, wait := l.Allow("ip:" + s.ClientIP(r)); !ok {
		return false, wait
	}
	if user != "" {
		if ok, wait := l.Allow("user:" + user); !ok {
			return false, wait
		}
	}
	return true, 0
}

// RetryAfter sets the Retry-After header (whole seconds, at least 1).
func RetryAfter(w http.ResponseWriter, wait time.Duration) {
	secs := int((wait + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}
