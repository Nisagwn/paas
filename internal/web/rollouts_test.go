package web_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/store"
)

func TestRolloutPanel(t *testing.T) {
	u := setup(t)
	u.login()
	ctx := context.Background()
	u.post("/apps", url.Values{"name": {"blog"}, "repo": {"nisagwn/blog"}, "csrf": {u.csrf("/")}}, "self")
	app, _ := u.st.GetAppByName(ctx, "blog")
	ready := func(n int) store.Deployment {
		u.st.EnqueueDeployment(ctx, app.ID, sha(n), "main", "")
		d, err := u.st.ClaimNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := u.st.MarkReady(ctx, d, []store.AliasSpec{
			{Hostname: "main-blog.paas.test", Kind: store.AliasPreview, Branch: "main"},
			{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"},
		}); err != nil {
			t.Fatal(err)
		}
		return d
	}
	ready(1)
	if _, body, _ := u.get("/apps/blog"); !strings.Contains(body, "Şu an süren bir yayın yok") || !strings.Contains(body, "Anında") {
		t.Fatalf("idle panel:\n%s", body)
	}

	// Settings form: minutes and comma decimals; invalid values are refused.
	csrf := u.csrf("/apps/blog")
	form := url.Values{"csrf": {csrf}, "mode": {"canary"}, "steps": {"25, 100"}, "step_minutes": {"2"},
		"guard_minutes": {"10"}, "max_error_pct": {"2,5"}, "max_error_increase_pct": {"1"}, "max_p95_ms": {"0"},
		"max_p95_factor": {"2"}, "min_requests": {"20"}}
	if code, _, h := u.post("/apps/blog/rollout-settings", form, "self"); code != http.StatusSeeOther ||
		!strings.Contains(h.Get("Location"), "ok=rollout-settings") {
		t.Fatalf("settings: %d %v", code, h)
	}
	s, _ := u.st.GetRolloutSettings(ctx, app.ID)
	if s.Mode != store.RolloutCanary || store.FormatSteps(s.Steps) != "25,100" || s.StepSeconds != 120 || s.MaxErrorPct != 2.5 || s.MinRequests != 20 {
		t.Fatalf("stored: %+v", s)
	}
	bad := url.Values{"csrf": {csrf}, "mode": {"canary"}, "steps": {"25,100"}, "step_minutes": {"1"}}
	if code, body, _ := u.post("/apps/blog/rollout-settings", bad, "self"); code != http.StatusBadRequest || !strings.Contains(body, "Geçersiz ayar") {
		t.Fatalf("invalid settings: %d", code)
	}

	// A canary shows progress, both sides and the buttons; the deployment
	// list marks it.
	d2 := ready(2)
	_, body, _ := u.get("/apps/blog")
	for _, want := range []string{"trafiğin %25 payını alıyor", "Hemen tamamla", ">Durdur<", ">Geri al<", "Canary %25", "Yeni deploy", "Sıradaki karar"} {
		if !strings.Contains(body, want) {
			t.Fatalf("panel lacks %q:\n%s", want, body)
		}
	}
	if code, _, h := u.post("/apps/blog/rollout/pause", url.Values{"csrf": {csrf}}, "self"); code != http.StatusSeeOther ||
		!strings.Contains(h.Get("Location"), "ok=rollout") {
		t.Fatalf("pause: %d", code)
	}
	if _, body, _ = u.get("/apps/blog"); !strings.Contains(body, ">Sürdür<") || !strings.Contains(body, "Duraklatıldı") {
		t.Fatal("paused panel")
	}
	if code, _, _ := u.post("/apps/blog/rollout/rollback", url.Values{"csrf": {csrf}}, "self"); code != http.StatusSeeOther {
		t.Fatalf("rollback: %d", code)
	}
	_, body, _ = u.get("/apps/blog")
	if !strings.Contains(body, "Canary geri alındı") || !strings.Contains(body, "Şu an süren bir yayın yok") {
		t.Fatalf("rolled back canary not marked:\n%s", body)
	}
	if code, _, _ := u.post("/apps/blog/rollout/promote", url.Values{"csrf": {csrf}}, "self"); code != http.StatusNotFound {
		t.Fatalf("no active rollout: %d", code)
	}
	if code, _, _ := u.post("/apps/blog/rollout/explode", url.Values{"csrf": {csrf}}, "self"); code != http.StatusNotFound {
		t.Fatalf("unknown action: %d", code)
	}
	r, _ := u.st.ListRollouts(ctx, app.ID, 1)
	if len(r) != 1 || r[0].ToDeploymentID != d2.ID || r[0].State != store.RolloutRolledBack {
		t.Fatalf("rollouts: %+v", r)
	}
}
