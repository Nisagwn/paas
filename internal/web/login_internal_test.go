package web

import (
	"strings"
	"testing"
	"time"
)

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "/",
		"/":                             "/",
		"/apps/blog":                    "/apps/blog",
		"/import?team=web#repos":        "/import?team=web#repos",
		"/teams/web/":                   "/teams/web/",
		"//evil.example":                "/",
		"///evil.example":               "/",
		"/\\evil.example":               "/",
		"/a\\b":                         "/",
		"\\\\evil.example":              "/",
		"/\t/evil.example":              "/",
		"/\r\n/evil.example":            "/",
		"/\x00x":                        "/",
		"/%2F%2Fevil.example":           "/",
		"https://evil.example/":         "/",
		"HTTPS://evil.example/":         "/",
		"javascript:alert(1)":           "/",
		"evil.example":                  "/",
		"/" + strings.Repeat("a", 3000): "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgeLabel(t *testing.T) {
	for d, want := range map[time.Duration]string{30 * 24 * time.Hour: "30 gün", 36 * time.Hour: "1 gün", 5 * time.Hour: "5 saat", time.Minute: "1 saat"} {
		if got := ageLabel(d); got != want {
			t.Errorf("ageLabel(%s) = %q, want %q", d, got, want)
		}
	}
}
