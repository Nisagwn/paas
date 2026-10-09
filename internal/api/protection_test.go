package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/protect"
)

func TestProtectionAPI(t *testing.T) {
	protect.Iterations = 1000
	f := setupTeams(t)

	code, body := f.call("bob", "GET", "/api/apps/blog/protection", nil)
	if code != 200 || !strings.Contains(body, `"mode":"off"`) || !strings.Contains(body, `"scope":"previews"`) ||
		!strings.Contains(body, `"password_set":false`) {
		t.Fatalf("default: %d %s", code, body)
	}
	for _, bad := range []map[string]string{
		{"mode": "everyone"}, {"scope": "some"}, {"mode": "password"}, {"mode": "password", "password": "short"},
	} {
		if code, body := f.call("bob", "PUT", "/api/apps/blog/protection", bad); code != 400 {
			t.Errorf("%v: %d %s", bad, code, body)
		}
	}
	code, body = f.call("bob", "PUT", "/api/apps/blog/protection",
		map[string]string{"mode": "password", "scope": "all", "password": "correct horse"})
	if code != 200 || !strings.Contains(body, `"password_set":true`) || strings.Contains(body, "pbkdf2") ||
		strings.Contains(body, "correct horse") {
		t.Fatalf("set password: %d %s", code, body)
	}
	// Mode change keeps the password; empty fields keep their value.
	code, body = f.call("bob", "PUT", "/api/apps/blog/protection", map[string]string{"mode": "team"})
	if code != 200 || !strings.Contains(body, `"scope":"all"`) || !strings.Contains(body, `"password_set":true`) {
		t.Fatalf("mode only: %d %s", code, body)
	}

	// Share links: the token is shown once, the listing has its prefix.
	code, body = f.call("bob", "POST", "/api/apps/blog/protection/share-links",
		map[string]any{"name": "review", "expires_in_hours": 24, "url": "https://blog." + domain + "/docs"})
	var created struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	json.Unmarshal([]byte(body), &created)
	if code != 201 || !strings.HasPrefix(created.Token, protect.SharePrefix) ||
		created.URL != "https://blog."+domain+"/docs?paas_share="+created.Token {
		t.Fatalf("create share: %d %s", code, body)
	}
	if code, body := f.call("bob", "POST", "/api/apps/blog/protection/share-links", map[string]string{"url": "https://evil.example/"}); code != 400 {
		t.Fatalf("foreign url: %d %s", code, body)
	}
	_, body = f.call("carol", "GET", "/api/apps/blog/protection", nil)
	if !strings.Contains(body, `"name":"review"`) || strings.Contains(body, created.Token) {
		t.Fatalf("listing: %s", body)
	}
	if code, _ := f.call("bob", "DELETE", "/api/apps/blog/protection/share-links/"+itoa(created.ID), nil); code != 204 {
		t.Fatalf("delete share: %d", code)
	}

	// Bypass secret.
	code, body = f.call("bob", "POST", "/api/apps/blog/protection/bypass", nil)
	if code != 201 || !strings.Contains(body, protect.BypassPrefix) || !strings.Contains(body, protect.BypassHeader) {
		t.Fatalf("bypass: %d %s", code, body)
	}
	_, body = f.call("carol", "GET", "/api/apps/blog/protection", nil)
	if !strings.Contains(body, `"bypass":{"prefix":"paas_bypass_`) {
		t.Fatalf("bypass listing: %s", body)
	}
	f.call("bob", "DELETE", "/api/apps/blog/protection/bypass", nil)
	_, body = f.call("carol", "GET", "/api/apps/blog/protection", nil)
	if !strings.Contains(body, `"bypass":null`) {
		t.Fatalf("bypass removed: %s", body)
	}
}
