package web

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

func TestParseDotenv(t *testing.T) {
	got, msg := parseDotenv("# yorum\r\n\nexport API_KEY=abc\nDATABASE_URL=\"postgres://x?a=1\"\nMULTI=\"a\\nb\"\nSINGLE='$keep'\nEMPTY=\nTRAIL=val # yorum\nAPI_KEY=son\n")
	if msg != "" {
		t.Fatal(msg)
	}
	want := map[string]string{"API_KEY": "son", "DATABASE_URL": "postgres://x?a=1", "MULTI": "a\nb", "SINGLE": "$keep", "EMPTY": "", "TRAIL": "val"}
	if len(got) != len(want) {
		t.Fatalf("got %d changes: %+v", len(got), got)
	}
	for _, c := range got {
		if c.Value == nil || *c.Value != want[c.Key] {
			t.Errorf("%s = %v, want %q", c.Key, c.Value, want[c.Key])
		}
	}
	if got[0].Key != "API_KEY" {
		t.Errorf("order: first is %s", got[0].Key)
	}
	for in, wantMsg := range map[string]string{
		"":                  "değişken bulunamadı",
		"# yalnızca yorum":  "değişken bulunamadı",
		"OK=1\nbozuk satır": "2. satır: AD=değer biçiminde değil",
		"1ABC=x":            "1. satır: Geçersiz değişken adı",
		"PORT=80":           "1. satır: PORT platform tarafından ayarlanır",
	} {
		if _, msg := parseDotenv(in); !strings.Contains(msg, wantMsg) {
			t.Errorf("parseDotenv(%q) = %q, want %q", in, msg, wantMsg)
		}
	}
	var many strings.Builder
	for i := 0; i < 201; i++ {
		fmt.Fprintf(&many, "K%d=1\n", i)
	}
	if _, msg := parseDotenv(many.String()); msg != envBulkTooMany {
		t.Errorf("too many: %q", msg)
	}
}

func TestFailureKind(t *testing.T) {
	for in, want := range map[string]string{
		"build: buildctl: exit status 1":                                  "build",
		"build: fetch source: git fetch: repository not found":            "fetch",
		`build: root directory "web" not found in the repository`:         "root",
		"deploy: not ready after 3m0s (deadline exceeded): CrashLoopBack": "timeout",
		"deploy: rollout stalled: ProgressDeadlineExceeded":               "start",
		"deploy: cannot create pods: exceeded quota":                      "quota",
		"databases: copy failed":                                          "database",
		"deploy: kubernetes API unreachable: dial tcp":                    "platform",
		"something odd": "other",
	} {
		if got := failureKind(store.Deployment{Status: store.StatusFailed, Error: in}); got != want {
			t.Errorf("failureKind(%q) = %q, want %q", in, got, want)
		}
	}
	if got := failureKind(store.Deployment{Status: store.StatusCanceled}); got != "canceled" {
		t.Errorf("canceled: %q", got)
	}
}

func TestTimeline(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(s int) *time.Time { v := t0.Add(time.Duration(s) * time.Second); return &v }
	states := func(steps []timelineStep) string {
		var out []string
		for _, s := range steps {
			out = append(out, s.State+":"+s.Duration)
		}
		return strings.Join(out, " ")
	}
	cases := []struct {
		name     string
		d        store.Deployment
		deployAt *time.Time
		want     string
	}{
		{"queued", store.Deployment{Status: store.StatusQueued, CreatedAt: t0},
			nil, "current: pending: pending: pending:"},
		{"building", store.Deployment{Status: store.StatusBuilding, CreatedAt: t0, StartedAt: at(2)},
			nil, "done:2 sn current: pending: pending:"},
		{"deploying", store.Deployment{Status: store.StatusDeploying, CreatedAt: t0, StartedAt: at(2)},
			at(32), "done:2 sn done:30 sn current: pending:"},
		{"ready", store.Deployment{Status: store.StatusReady, CreatedAt: t0, StartedAt: at(2), FinishedAt: at(47)},
			at(32), "done:2 sn done:30 sn done:15 sn done:toplam 47 sn"},
		{"build failed", store.Deployment{Status: store.StatusFailed, Error: "build: buildctl: exit status 1", CreatedAt: t0, StartedAt: at(1), FinishedAt: at(61)},
			nil, "done:1 sn failed:1 dk pending: failed:toplam 1 dk 1 sn"},
		{"start failed", store.Deployment{Status: store.StatusFailed, Error: "deploy: rollout stalled: x", CreatedAt: t0, StartedAt: at(1), FinishedAt: at(91)},
			at(31), "done:1 sn done:30 sn failed:1 dk failed:toplam 1 dk 31 sn"},
		{"canceled in queue", store.Deployment{Status: store.StatusCanceled, CreatedAt: t0, FinishedAt: at(5)},
			nil, "skipped: pending: pending: skipped:toplam 5 sn"},
	}
	for _, c := range cases {
		f := explainFailure(store.App{Name: "blog"}, c.d, false)
		if got := states(timeline(c.d, c.deployAt, false, f)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}
