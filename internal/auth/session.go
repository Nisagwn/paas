// Package auth implements authentication for the API and the web UI:
// browser sessions, personal API tokens, the legacy admin token, and the
// principal (who is calling) that authorization decisions are made for.
//
// A browser session is a stateless, signed cookie:
//
//	paas_session = <expiry unix>.<user id>.<random nonce>.<HMAC-SHA256>
//
// User id 0 is the admin principal (token login, dev mode only). The HMAC
// key comes from PAAS_SESSION_KEY, or is derived from the API token when
// that is unset, so sessions survive restarts, work across replicas without
// shared state, and all become invalid when the key is rotated. The CSRF
// token for forms is HMAC(key, "csrf"+nonce): bound to the session and
// never stored.
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
func New(apiToken string) *Sessions { return NewWithKey("", apiToken) }

// NewWithKey signs sessions with sessionKey (PAAS_SESSION_KEY), or with a
// key derived from apiToken when sessionKey is empty. apiToken is also the
// legacy admin token accepted by CheckToken ("" disables it).
func NewWithKey(sessionKey, apiToken string) *Sessions {
	secret, label := apiToken, "paas session key v2"
	if sessionKey != "" {
		secret, label = sessionKey, "paas session key v2 (dedicated)"
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(label))
	return &Sessions{token: apiToken, key: m.Sum(nil), TTL: DefaultTTL, now: time.Now}
}

// CheckToken compares a presented legacy API token in constant time.
func (s *Sessions) CheckToken(token string) bool {
	return token != "" && s.token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) == 1
}

// HasToken reports whether a legacy API token is configured.
func (s *Sessions) HasToken() bool { return s.token != "" }

func (s *Sessions) mac(parts ...string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(strings.Join(parts, ".")))
	return m.Sum(nil)
}

// Issue sets a new admin session cookie (token login).
func (s *Sessions) Issue(w http.ResponseWriter, r *http.Request) { s.IssueUser(w, r, 0) }

// IssueUser sets a new session cookie for a user (0: admin).
func (s *Sessions) IssueUser(w http.ResponseWriter, r *http.Request, userID int64) {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	exp := strconv.FormatInt(s.now().Add(s.TTL).Unix(), 10)
	uid := strconv.FormatInt(userID, 10)
	n := b64.EncodeToString(nonce)
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: exp + "." + uid + "." + n + "." + b64.EncodeToString(s.mac("session", exp, uid, n)),
		Path: "/", MaxAge: int(s.TTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: IsTLS(r),
	})
}

// Clear deletes the session cookie.
func (s *Sessions) Clear(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: IsTLS(r),
	})
}

// parse returns the user id and nonce of a valid, unexpired session cookie.
func (s *Sessions) parse(r *http.Request) (int64, string, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return 0, "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 4 {
		return 0, "", false
	}
	sig, err := b64.DecodeString(parts[3])
	if err != nil || !hmac.Equal(sig, s.mac("session", parts[0], parts[1], parts[2])) {
		return 0, "", false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || s.now().Unix() >= exp {
		return 0, "", false
	}
	uid, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || uid < 0 {
		return 0, "", false
	}
	return uid, parts[2], true
}

func (s *Sessions) nonce(r *http.Request) (string, bool) {
	_, n, ok := s.parse(r)
	return n, ok
}

// Valid reports whether r carries a valid session cookie.
func (s *Sessions) Valid(r *http.Request) bool {
	_, ok := s.nonce(r)
	return ok
}

// User returns the user id of r's valid session (0: admin session).
func (s *Sessions) User(r *http.Request) (int64, bool) {
	uid, _, ok := s.parse(r)
	return uid, ok
}

// Seal signs value for purpose with an expiry; Open verifies it. Used for
// short-lived cookies such as the OAuth state.
func (s *Sessions) Seal(purpose, value string, ttl time.Duration) string {
	exp := strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	v := b64.EncodeToString([]byte(value))
	return exp + "." + v + "." + b64.EncodeToString(s.mac("seal", purpose, exp, v))
}

// Open returns the value of an unexpired string sealed for purpose.
func (s *Sessions) Open(purpose, sealed string) (string, bool) {
	parts := strings.Split(sealed, ".")
	if len(parts) != 3 {
		return "", false
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, s.mac("seal", purpose, parts[0], parts[1])) {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || s.now().Unix() >= exp {
		return "", false
	}
	v, err := b64.DecodeString(parts[1])
	return string(v), err == nil
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
// the form or header token must match the session. SameSite=Lax keeps the
// cookie off cross-site POSTs; this is the second layer. (Lax, not Strict:
// coming back from github.com after installing the App is a cross-site
// navigation, and Strict would sign the user out on every return.)
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
