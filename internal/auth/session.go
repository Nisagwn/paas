// Package auth implements browser sessions for the web UI.
//
// A user logs in once with the API token and receives a stateless, signed
// cookie:
//
//	paas_session = <expiry unix>.<random nonce, base64url>.<HMAC-SHA256, base64url>
//
// The HMAC key is derived from the API token, so sessions survive restarts,
// work across replicas without shared state, and all become invalid when
// the token is rotated. The CSRF token for forms is HMAC(key, "csrf"+nonce):
// bound to the session and never stored.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	CookieName = "paas_session"
	// CSRFField is the form field (or X-CSRF-Token header) carrying the token.
	CSRFField = "csrf"
	// DefaultTTL is how long a login lasts.
	DefaultTTL = 12 * time.Hour
)

var b64 = base64.RawURLEncoding

type Sessions struct {
	token string
	key   []byte
	TTL   time.Duration
	// now is replaced in tests.
	now func() time.Time
}

// New derives the signing key from the API token.
func New(apiToken string) *Sessions {
	m := hmac.New(sha256.New, []byte(apiToken))
	m.Write([]byte("paas session key v1"))
	return &Sessions{token: apiToken, key: m.Sum(nil), TTL: DefaultTTL, now: time.Now}
}

// CheckToken compares a presented API token in constant time.
func (s *Sessions) CheckToken(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) == 1
}

func (s *Sessions) mac(parts ...string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(strings.Join(parts, ".")))
	return m.Sum(nil)
}

// Issue sets a new session cookie.
func (s *Sessions) Issue(w http.ResponseWriter, r *http.Request) {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	exp := strconv.FormatInt(s.now().Add(s.TTL).Unix(), 10)
	n := b64.EncodeToString(nonce)
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: exp + "." + n + "." + b64.EncodeToString(s.mac(exp, n)),
		Path: "/", MaxAge: int(s.TTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: IsTLS(r),
	})
}

// Clear deletes the session cookie.
func (s *Sessions) Clear(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: IsTLS(r),
	})
}

// nonce returns the nonce of a valid, unexpired session cookie.
func (s *Sessions) nonce(r *http.Request) (string, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return "", false
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, s.mac(parts[0], parts[1])) {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || s.now().Unix() >= exp {
		return "", false
	}
	return parts[1], true
}

// Valid reports whether r carries a valid session cookie.
func (s *Sessions) Valid(r *http.Request) bool {
	_, ok := s.nonce(r)
	return ok
}

// CSRFToken returns the form token for r's session, or "" without one.
func (s *Sessions) CSRFToken(r *http.Request) string {
	n, ok := s.nonce(r)
	if !ok {
		return ""
	}
	return b64.EncodeToString(s.mac("csrf", n))
}

// CheckCSRF validates an unsafe request made with a session cookie: the
// Origin (or, failing that, Referer) must be this host when present, and
// the form or header token must match the session. SameSite=Strict already
// keeps the cookie off cross-site requests; this is the second layer.
func (s *Sessions) CheckCSRF(r *http.Request) bool {
	if !SameOrigin(r) {
		return false
	}
	want := s.CSRFToken(r)
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		got = r.PostFormValue(CSRFField)
	}
	return want != "" && hmac.Equal([]byte(got), []byte(want))
}

// SameOrigin rejects requests whose Origin or Referer names another host.
// Requests carrying neither (old browsers, curl) pass; the CSRF token still
// has to match.
func SameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// IsTLS reports whether the client connection is HTTPS, directly or via a
// TLS-terminating proxy such as Traefik.
func IsTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
