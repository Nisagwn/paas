package api_test

import (
	"testing"

	"github.com/nisagwn/paas/internal/worker"
)

func TestScaleToZeroAPI(t *testing.T) {
	e := setup(t, worker.DryRunPipeline{})
	e.do("POST", "/api/apps", map[string]string{"name": "blog", "repo": "nisagwn/blog"}, nil)

	var out map[string]any
	if code := e.do("GET", "/api/apps/blog/scale-to-zero", nil, &out); code != 200 || out["production"] != false {
		t.Fatalf("default: %d %v", code, out)
	}
	if code := e.do("PUT", "/api/apps/blog/scale-to-zero", map[string]any{"production": true}, &out); code != 200 ||
		out["production"] != true {
		t.Fatalf("put: %d %v", code, out)
	}
	if code := e.do("GET", "/api/apps/blog/scale-to-zero", nil, &out); code != 200 || out["production"] != true {
		t.Fatalf("after put: %d %v", code, out)
	}
	for _, bad := range []any{map[string]any{}, map[string]any{"production": "yes"}, map[string]any{"preview": true}} {
		if code := e.do("PUT", "/api/apps/blog/scale-to-zero", bad, nil); code != 400 {
			t.Errorf("%v: got %d, want 400", bad, code)
		}
	}
	if code := e.do("PUT", "/api/apps/nope/scale-to-zero", map[string]any{"production": true}, nil); code != 404 {
		t.Errorf("unknown app: got %d", code)
	}

	// Deployments carry the sleeping flag.
	e.push("nisagwn/blog", "main", sha(1))
	e.drain()
	var deps []map[string]any
	if code := e.do("GET", "/api/apps/blog/deployments", nil, &deps); code != 200 || len(deps) != 1 || deps[0]["sleeping"] != false {
		t.Fatalf("deployments: %d %v", code, deps)
	}
}
