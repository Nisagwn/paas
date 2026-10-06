package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

const rolloutSettingsJSON = `{"mode":"canary","steps":[10,50,100],"step_seconds":300,"max_error_pct":5,
	"max_error_increase_pct":2,"max_p95_ms":0,"max_p95_factor":2,"min_requests":50,"guard_seconds":600}`

func TestRolloutCommands(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.json("GET /api/apps/blog/rollout", 200, `{"settings":`+rolloutSettingsJSON+`,
		"active":{"id":3,"from_deployment_id":7,"to_deployment_id":9,"mode":"canary","step":1,"weight":50,"state":"running",
			"reason":"adım sürüyor","settings":`+rolloutSettingsJSON+`,"verdicts":[],"created_at":"2026-10-05T11:50:00Z",
			"metrics":{"canary":{"requests":120,"errors":1,"p95_ms":82},"production":{"requests":900,"errors":0},
				"next_action":"hold","next_reason":"adım sürüyor","elapsed_seconds":90,"duration_seconds":300}},
		"recent":[{"id":3,"to_deployment_id":9,"mode":"canary","state":"running","weight":50,"settings":`+rolloutSettingsJSON+`},
			{"id":2,"to_deployment_id":8,"mode":"canary","state":"rolled_back","reason":"5xx oranı %30.0, eşik %5.0",
			"finished_at":"2026-10-05T11:00:00Z","settings":`+rolloutSettingsJSON+`}]}`)
	res := run(t, loggedIn(f), "", "rollout", "status", "blog")
	for _, want := range []string{"#3 canary, running", "#9 (new) ← #7", "Weight:", "50% (step 2 of 10,50,100%)",
		"120 requests, 5xx 0.8%, p95 82 ms", "900 requests, 5xx 0.0%", "hold: adım sürüyor", "rolled_back", "1h ago"} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("status lacks %q:\n%s", want, res.stdout)
		}
	}

	f.json("POST /api/apps/blog/rollout/promote", 200, `{"id":3,"to_deployment_id":9,"mode":"canary","weight":100,"state":"promoted"}`)
	if res := run(t, loggedIn(f), "", "rollout", "promote", "blog"); res.code != ExitOK ||
		!strings.Contains(res.stdout, "Rollout #3 of blog (deployment #9): promoted, weight 100%.") {
		t.Fatalf("promote: %+v", res)
	}
	f.json("POST /api/apps/blog/rollout/pause", 404, `{"error":"no active rollout for this app"}`)
	if res := run(t, loggedIn(f), "", "rollout", "pause", "blog"); res.code != ExitError || !strings.Contains(res.stderr, "no active rollout") {
		t.Fatalf("pause: %+v", res)
	}

	// settings: only the given flags are sent.
	f.json("PUT /api/apps/blog/rollout-settings", 200, rolloutSettingsJSON)
	res = run(t, loggedIn(f), "", "rollout", "settings", "blog", "--mode", "canary", "--steps", "10,50,100", "--step", "5m", "--max-p95-ms", "800")
	if res.code != ExitOK || !strings.Contains(res.stdout, "Canary steps:") || !strings.Contains(res.stdout, "10,50,100%") {
		t.Fatalf("settings: %+v", res)
	}
	var body map[string]any
	if b := f.body("PUT /api/apps/blog/rollout-settings"); len(b) != 1 || json.Unmarshal([]byte(b[0]), &body) != nil ||
		len(body) != 4 || body["mode"] != "canary" || body["step_seconds"] != float64(300) || body["max_p95_ms"] != float64(800) {
		t.Fatalf("body %v", f.body("PUT /api/apps/blog/rollout-settings"))
	}
	f.json("GET /api/apps/blog/rollout-settings", 200, rolloutSettingsJSON)
	if res := run(t, loggedIn(f), "", "rollout", "settings", "blog"); res.code != ExitOK || !strings.Contains(res.stdout, "Mode:") {
		t.Fatalf("settings get: %+v", res)
	}

	for _, bad := range [][]string{
		{"rollout", "explode", "blog"},
		{"rollout", "status"},
		{"rollout", "promote", "blog", "--mode", "canary"},
		{"rollout", "settings", "blog", "--steps", "a,b"},
	} {
		if res := run(t, loggedIn(f), "", bad...); res.code != ExitUsage {
			t.Errorf("%v: %+v", bad, res)
		}
	}
}
