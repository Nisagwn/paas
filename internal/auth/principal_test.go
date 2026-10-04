package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUserSessions(t *testing.T) {
	s := NewWithKey(strings.Repeat("k", 32), "legacy")
	rec := httptest.NewRecorder()
	s.IssueUser(rec, httptest.NewRequest("GET", "/", nil), 42)
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(rec.Result().Cookies()[0])
	if uid, ok := s.User(r); !ok || uid != 42 {
		t.Fatalf("User = %d %v", uid, ok)
	}
	// A dedicated key is independent of the API token.
	if New("legacy").Valid(r) {
		t.Fatal("session accepted under a key derived from the token")
	}
	if !NewWithKey(strings.Repeat("k", 32), "rotated").Valid(r) {
		t.Fatal("rotating the API token must not end sessions signed with PAAS_SESSION_KEY")
	}
	if !s.CheckToken("legacy") || s.CheckToken("") || New("").CheckToken("") {
		t.Fatal("legacy token check")
	}
	if New("").HasToken() || !s.HasToken() {
		t.Fatal("HasToken")
	}
}

func TestSeal(t *testing.T) {
	s := New("token")
	now := time.Now()
	s.now = func() time.Time { return now }
	sealed := s.Seal("oauth", "state verifier /apps", time.Minute)
	if v, ok := s.Open("oauth", sealed); !ok || v != "state verifier /apps" {
		t.Fatalf("Open = %q %v", v, ok)
	}
	if _, ok := s.Open("other", sealed); ok {
		t.Fatal("sealed value accepted for another purpose")
	}
	if _, ok := New("other").Open("oauth", sealed); ok {
		t.Fatal("sealed value accepted under another key")
	}
	if _, ok := s.Open("oauth", sealed[:len(sealed)-2]+"xx"); ok {
		t.Fatal("tampered value accepted")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := s.Open("oauth", sealed); ok {
		t.Fatal("expired value accepted")
	}
}

func TestAPITokenFormat(t *testing.T) {
	plain, hash, prefix := NewAPIToken()
	if !strings.HasPrefix(plain, TokenPrefix) || len(plain) != len(TokenPrefix)+43 {
		t.Fatalf("token %q", plain)
	}
	if hash != HashToken(plain) || len(hash) != 64 || strings.Contains(hash, plain) {
		t.Fatalf("hash %q", hash)
	}
	if !strings.HasPrefix(plain, prefix) || len(prefix) != len(TokenPrefix)+6 {
		t.Fatalf("prefix %q", prefix)
	}
	other, _, _ := NewAPIToken()
	if other == plain {
		t.Fatal("tokens must be random")
	}
	// RFC 7636 appendix B test vector.
	if got := PKCEChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("PKCE challenge = %s", got)
	}
}
