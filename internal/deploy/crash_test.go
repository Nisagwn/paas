package deploy

import (
	"strings"
	"testing"
)

func TestCrashSummary(t *testing.T) {
	// A real Node crash: the cause is far above the last lines.
	node := []string{
		"/app/server.js:12",
		"this is not javascript",
		"     ^^",
		"",
		"SyntaxError: Unexpected identifier 'is'",
		"    at wrapSafe (node:internal/modules/cjs/loader:1486:18)",
		"    at Module._compile (node:internal/modules/cjs/loader:1528:20)",
		"    at node:internal/main/run_main_module:36:49",
		"",
		"Node.js v22.23.3",
	}
	got := crashSummary(node)
	if !strings.HasPrefix(got, "error: SyntaxError: Unexpected identifier 'is'; last log lines:") ||
		!strings.HasSuffix(got, "Node.js v22.23.3") {
		t.Fatalf("got %q", got)
	}

	// The error is already in the tail: not repeated.
	goPanic := []string{"listening", "panic: runtime error: index out of range", "goroutine 1 [running]:"}
	if got := crashSummary(goPanic); strings.Count(got, "panic:") != 1 || !strings.HasPrefix(got, "last log lines:") {
		t.Fatalf("got %q", got)
	}

	// No recognizable error line: just the tail.
	if got := crashSummary([]string{"a", "b", "c", "d"}); got != "last log lines: b ⏎ c ⏎ d" {
		t.Fatalf("got %q", got)
	}
}
