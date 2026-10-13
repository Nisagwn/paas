package cli

import (
	"net/http"
	"strings"
	"testing"
)

// Faz 28: `paas apps delete` asks for the name unless --yes, sends it as
// the confirmation and reports 204 and 202 answers.
func TestAppsDelete(t *testing.T) {
	isolate(t)
	f := newFakeAPI(t)
	f.mux.HandleFunc("DELETE /api/apps/blog", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	f.json("DELETE /api/apps/shop", 202, `{"app":"shop","status":"deleting","deletion_id":3}`)

	// Wrong name typed: nothing is sent.
	res := run(t, loggedIn(f), "blgo\n", "apps", "delete", "blog")
	if res.code != ExitError || !strings.Contains(res.stderr, "does not match") || len(f.body("DELETE /api/apps/blog")) != 0 {
		t.Fatalf("mismatch: %+v", res)
	}
	// No stdin: hint at --yes.
	if res := run(t, loggedIn(f), "", "apps", "delete", "blog"); res.code != ExitError || !strings.Contains(res.stderr, "--yes") {
		t.Fatalf("no stdin: %+v", res)
	}
	res = run(t, loggedIn(f), "blog\n", "apps", "delete", "blog")
	if res.code != ExitOK || !strings.Contains(res.stdout, "Deleted blog.") || !strings.Contains(res.stderr, "cannot be undone") {
		t.Fatalf("typed: %+v", res)
	}
	if b := f.body("DELETE /api/apps/blog"); len(b) != 1 || b[0] != `{"confirm":"blog"}` {
		t.Errorf("body %v", b)
	}
	res = run(t, loggedIn(f), "", "apps", "delete", "shop", "--yes")
	if res.code != ExitOK || !strings.Contains(res.stdout, "in the background") {
		t.Fatalf("--yes 202: %+v", res)
	}
	res = run(t, loggedIn(f), "", "--json", "apps", "rm", "blog", "--yes")
	if res.code != ExitOK || !strings.Contains(res.stdout, `"status": "deleted"`) {
		t.Fatalf("json: %+v", res)
	}
	if res := run(t, loggedIn(f), "", "apps", "frobnicate", "blog"); res.code != ExitUsage {
		t.Fatalf("unknown subcommand: %+v", res)
	}
}
