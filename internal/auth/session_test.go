package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// login returns a request carrying a freshly issued session cookie.
func login(t *testing.T, s *Sessions) *http.Request {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Issue(rec, httptest.NewRequest("POST", "/login", nil))
	c := rec.Result().Cookies()
	if len(c) != 1 || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", c)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c[0])
	return r
}

func TestSessionLifecycle(t *testing.T) {
	s := New("token-1")
	now := time.Now()
	s.now = func() time.Time { return now }
	r := login(t, s)
	if !s.Valid(r) {
		t.Fatal("fresh session must be valid")
	}

	// Another token (rotation) invalidates every session.
	if New("token-2").Valid(r) {
		t.Fatal("session signed with an old token must be rejected")
	}

	// Tampering with expiry, user id or nonce breaks the signature.
	c, _ := r.Cookie(CookieName)
	parts := strings.Split(c.Value, ".")
	for _, v := range []string{
		"9999999999." + parts[1] + "." + parts[2] + "." + parts[3],
		parts[0] + ".7." + parts[2] + "." + parts[3],
		parts[0] + "." + parts[1] + ".x." + parts[3],
		parts[0] + "." + parts[2] + "." + parts[3], // old 3-part format
		"garbage",
	} {
		bad := httptest.NewRequest("GET", "/", nil)
		bad.AddCookie(&http.Cookie{Name: CookieName, Value: v})
		if s.Valid(bad) {
			t.Fatalf("tampered cookie %q accepted", v)
		}
	}

	now = now.Add(DefaultTTL + time.Second)
	if s.Valid(r) {
		t.Fatal("expired session accepted")
	}
}

func TestCSRF(t *testing.T) {
	s := New("token")
	r := login(t, s)
	token := s.CSRFToken(r)
	if token == "" {
		t.Fatal("no CSRF token for a valid session")
	}
	c, _ := r.Cookie(CookieName)

	post := func(csrf, origin string) *http.Request {
		form := url.Values{CSRFField: {csrf}}
		req := httptest.NewRequest("POST", "http://paas.test/apps/blog/rollback", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(c)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		return req
	}
	if !s.CheckCSRF(post(token, "http://paas.test")) {
		t.Fatal("valid token from the same origin rejected")
	}
	if !s.CheckCSRF(post(token, "")) {
		t.Fatal("valid token without Origin rejected")
	}
	if s.CheckCSRF(post(token, "https://evil.example")) {
		t.Fatal("cross-origin request accepted")
	}
	if s.CheckCSRF(post("forged", "http://paas.test")) {
		t.Fatal("wrong token accepted")
	}
	// A token is bound to its session: another login's token does not fit.
	if other := s.CSRFToken(login(t, s)); s.CheckCSRF(post(other, "")) {
		t.Fatal("token of another session accepted")
	}
}

func TestIsTLS(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if IsTLS(r) {
		t.Fatal("plain request reported as TLS")
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if !IsTLS(r) {
		t.Fatal("request behind a TLS proxy not detected")
	}
}
