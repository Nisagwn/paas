package api_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

// settingsPipeline records the settings the worker hands to the builder
// and reports a framework.
type settingsPipeline struct {
	worker.DryRunPipeline
	mu   sync.Mutex
	seen []store.BuildSettings
}

func (p *settingsPipeline) Build(_ context.Context, d store.Deployment, s store.BuildSettings, _ worker.Logger) (worker.BuildResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, s)
	return worker.BuildResult{Image: "registry.local/" + d.AppName + ":" + d.CommitSHA, Framework: "Next.js"}, nil
}

func TestBuildSettingsAPI(t *testing.T) {
	p := &settingsPipeline{}
	e := setup(t, p)
	e.do("POST", "/api/apps", map[string]string{"name": "web", "repo": "nisagwn/web"}, nil)

	var out map[string]any
	if code := e.do("GET", "/api/apps/web/settings", nil, &out); code != 200 ||
		out["root_directory"] != "" || out["framework"] != "" || out["detected_framework"] != "" {
		t.Fatalf("default: %d %v", code, out)
	}
	if fws, _ := out["frameworks"].([]any); len(fws) < 10 {
		t.Fatalf("frameworks = %v", out["frameworks"])
	}

	// Values are cleaned up before they are stored.
	code := e.do("PUT", "/api/apps/web/settings", map[string]string{
		"root_directory": " ./apps/web/ ", "framework": "nextjs", "build_command": "npm run build:prod",
		"node_version": "v20",
	}, &out)
	if code != 200 || out["root_directory"] != "apps/web" || out["framework"] != "nextjs" ||
		out["node_version"] != "20" || out["updated_at"] == nil {
		t.Fatalf("put: %d %v", code, out)
	}
	// A partial update keeps the other fields; "" resets one.
	code = e.do("PUT", "/api/apps/web/settings", map[string]string{"build_command": "", "output_directory": "dist"}, &out)
	if code != 200 || out["root_directory"] != "apps/web" || out["build_command"] != "" || out["output_directory"] != "dist" {
		t.Fatalf("merge: %d %v", code, out)
	}

	for _, bad := range []map[string]any{
		{"root_directory": "/srv/app"},
		{"root_directory": "apps/../../etc"},
		{"root_directory": ".."},
		{"output_directory": "../out"},
		{"root_directory": `apps\web`},
		{"framework": "rails"},
		{"node_version": "latest-ish"},
		{"build_command": "npm run build\nFROM scratch"},
		{"start_command": "node server.js \\"},
		{"install_command": strings.Repeat("x", 2000)},
		{"unknown": "x"},
		{"framework": 3},
	} {
		if code := e.do("PUT", "/api/apps/web/settings", bad, nil); code != 400 {
			t.Errorf("%v: got %d, want 400", bad, code)
		}
	}
	if code := e.do("GET", "/api/apps/web/settings", nil, &out); out["root_directory"] != "apps/web" {
		t.Fatalf("a rejected update changed the settings: %d %v", code, out)
	}
	if code := e.do("PUT", "/api/apps/nope/settings", map[string]string{"framework": "vite"}, nil); code != 404 {
		t.Errorf("unknown app: %d", code)
	}

	// The worker passes the settings to the builder and records the framework.
	e.push("nisagwn/web", "main", sha(1))
	e.drain()
	if len(p.seen) != 1 || p.seen[0].RootDirectory != "apps/web" || p.seen[0].Framework != "nextjs" ||
		p.seen[0].OutputDirectory != "dist" {
		t.Fatalf("builder got %+v", p.seen)
	}
	var deps []map[string]any
	if e.do("GET", "/api/apps/web/deployments", nil, &deps); len(deps) != 1 || deps[0]["framework"] != "Next.js" {
		t.Fatalf("deployments: %v", deps)
	}
	if e.do("GET", "/api/apps/web/settings", nil, &out); out["detected_framework"] != "Next.js" {
		t.Fatalf("detected framework: %v", out)
	}
}
