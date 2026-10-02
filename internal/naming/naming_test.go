package naming

import (
	"strings"
	"testing"
)

const sha = "a3f9c1e8b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2"

func TestHosts(t *testing.T) {
	if got := DeploymentHost(sha, "blog", "paas.dev"); got != "a3f9c1e-blog.paas.dev" {
		t.Errorf("DeploymentHost = %q", got)
	}
	if got := ProductionHost("blog", "paas.dev"); got != "blog.paas.dev" {
		t.Errorf("ProductionHost = %q", got)
	}
	if got := PreviewHost("feature/New_Login", "blog", "paas.dev"); got != "feature-new-login-blog.paas.dev" {
		t.Errorf("PreviewHost = %q", got)
	}
	if got := PreviewHost("///", "blog", "paas.dev"); got != "branch-blog.paas.dev" {
		t.Errorf("PreviewHost(empty slug) = %q", got)
	}
}

func TestPreviewHostLabelLimit(t *testing.T) {
	app := "my-app"
	host := PreviewHost(strings.Repeat("very-long-branch-", 10), app, "paas.dev")
	label, _, _ := strings.Cut(host, ".")
	if len(label) > 63 {
		t.Fatalf("label is %d chars, want <= 63: %q", len(label), label)
	}
	if !strings.HasSuffix(label, "-"+app) || strings.Contains(label, "--") {
		t.Fatalf("unexpected label %q", label)
	}
}

func TestValidAppName(t *testing.T) {
	for name, want := range map[string]bool{
		"blog": true, "my-app2": true, "a": false, "Blog": false, "2app": false,
		"app-": false, "my_app": false, strings.Repeat("a", 32): false,
	} {
		if got := ValidAppName(name); got != want {
			t.Errorf("ValidAppName(%q) = %v, want %v", name, got, want)
		}
	}
}
