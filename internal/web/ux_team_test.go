package web_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/protect"
	"github.com/nisagwn/paas/internal/store"
)

// The team page explains roles, guards the last owner, and every member
// action ends with its own message.
func TestTeamPageUX(t *testing.T) {
	f := planUI(t, nil)
	ctx := context.Background()
	bob, _ := f.st.UpsertUser(ctx, 7, "bob", "Bob", "")

	_, body, _ := f.owner.get("/teams")
	mustContain(t, "teams list", body, `href="/teams/web"`, "2 üye", "1 proje", "Rolün: Sahip", "Hobby", `id="new-team"`)

	_, body, _ = f.owner.get("/teams/web")
	mustContain(t, "owner team page", body, "Roller ne yapabilir?", "Üye eklemek, rol değiştirmek, üye çıkarmak",
		`aria-current="page" href="/teams/web"`, `href="/teams/web/notifications"`, "Ekibin tek sahibisin",
		"GitHub hesabı bağla", "GitHub uygulaması bu sunucuda yapılandırılmamış", `name="op" value="role"`,
		"hx-confirm=\"@carol ekipten çıkarılsın mı?")
	mustNotContain(t, "owner team page", body, `hx-confirm="Web ekibinden ayrılmak`)

	_, body, _ = f.viewer.get("/teams/web")
	mustContain(t, "viewer team page", body, "Üye eklemeyi yalnızca ekip sahipleri yapabilir", "@alice",
		"GitHub uygulaması bu sunucuda yapılandırılmamış", `hx-confirm="Web ekibinden ayrılmak`)
	mustNotContain(t, "viewer team page", body, `action="/teams/web/members"`)

	code, _, h := form(f.owner, "/teams/web", "/teams/web/members", url.Values{"login": {"bob"}, "role": {"viewer"}})
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "ok=member-added") {
		t.Fatalf("add: %d %s", code, h.Get("Location"))
	}
	code, _, h = form(f.owner, "/teams/web", "/teams/web/members", url.Values{"login": {"bob"}, "role": {"owner"}, "op": {"role"}})
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "ok=member-role") {
		t.Fatalf("role: %d %s", code, h.Get("Location"))
	}
	_, body, _ = f.owner.get("/teams/web?ok=member-role")
	mustContain(t, "after role", body, "Rol değiştirildi.")
	mustNotContain(t, "after role", body, "Ekibin tek sahibisin")

	// A bad login keeps the invite form open with the value.
	code, body, _ = form(f.owner, "/teams/web", "/teams/web/members", url.Values{"login": {"not valid!"}, "role": {"member"}})
	if code != http.StatusBadRequest {
		t.Fatalf("bad login: %d", code)
	}
	mustContain(t, "bad login", body, `value="not valid!"`, "Geçersiz GitHub kullanıcı adı.")

	code, _, h = form(f.owner, "/teams/web", "/teams/web/members/remove", url.Values{"login": {"bob"}})
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "ok=member-removed") {
		t.Fatalf("remove: %d %s", code, h.Get("Location"))
	}
	// htmx removals navigate with HX-Redirect.
	req, _ := http.NewRequest("POST", f.ui.srv.URL+"/teams/web/members/remove",
		strings.NewReader(url.Values{"login": {"carol"}, "csrf": {f.viewer.csrf("/teams/web")}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", f.ui.srv.URL)
	req.Header.Set("HX-Request", "true")
	resp, err := f.viewer.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("HX-Redirect") != "/teams?ok=left-team" {
		t.Fatalf("leave: %d %s", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	_ = bob
}

// Tokens: a chosen expiry, the token shown once with a copy button, and
// the list's state column.
func TestTokensPageUX(t *testing.T) {
	f := planUI(t, nil)
	ctx := context.Background()
	alice, _ := f.st.UserByLogin(ctx, "alice")

	_, body, _ := f.owner.get("/tokens")
	mustContain(t, "empty tokens", body, "Henüz token yok.", `<option value="90" selected>90 gün</option>`, "Süresiz")

	code, body, _ := form(f.owner, "/tokens", "/tokens", url.Values{"name": {"ci"}, "expires_in_days": {"30"}})
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	mustContain(t, "created token", body, "Yeni token'ın hazır", "bir daha gösterilmez", `id="copy-token"`,
		"paas login --url", `readonly value="paas_`)
	toks, _ := f.st.APITokens(ctx, alice.ID)
	if len(toks) != 1 || toks[0].ExpiresAt == nil || toks[0].ExpiresAt.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("token: %+v", toks)
	}

	// An expired token is labelled.
	_, hash, prefix := auth.NewAPIToken()
	past := time.Now().Add(-time.Hour)
	if _, err := f.st.CreateAPIToken(ctx, alice.ID, "old", hash, prefix, &past); err != nil {
		t.Fatal(err)
	}
	_, body, _ = f.owner.get("/tokens")
	mustContain(t, "token list", body, "Süresi doldu", "Hiç kullanılmadı", "tarihine kadar", `hx-confirm="&quot;ci&quot; token`)
	mustNotContain(t, "token list", body, `readonly value="paas_`)

	// A bad name keeps the chosen expiry.
	code, body, _ = form(f.owner, "/tokens", "/tokens", url.Values{"name": {""}, "expires_in_days": {"7"}})
	if code != http.StatusBadRequest {
		t.Fatalf("empty name: %d", code)
	}
	mustContain(t, "bad token", body, `<option value="7" selected>7 gün</option>`)

	code, _, h := form(f.owner, "/tokens", "/tokens/revoke", url.Values{"id": {fmt.Sprint(toks[0].ID)}})
	if code != http.StatusSeeOther || h.Get("Location") != "/tokens?ok=revoked" {
		t.Fatalf("revoke: %d %s", code, h.Get("Location"))
	}
}

// Usage: percentages, levels, reset dates and how to get more.
func TestUsagePageUX(t *testing.T) {
	f := planUI(t, nil)
	_, body, _ := f.viewer.get("/usage")
	mustContain(t, "usage", body, "%100", "Limitte", "Yeni proje oluşturulamaz", "Sıfırlanma:", "UTC",
		"mailto:kvkk@example.com", "Ekip en az bir limite ulaştı", "Plan ve kullanım")
	mustNotContain(t, "viewer usage", body, "Planı Yönetim'de değiştir")
	_, body, _ = f.owner.get("/usage")
	mustContain(t, "admin usage", body, "/admin?q=web#team-web")
}

// Admin: search, filters, the inline plan form keeps overrides, a
// suspension needs a reason.
func TestAdminPageUX(t *testing.T) {
	f := planUI(t, nil)
	ctx := context.Background()
	dave, _ := f.st.UpsertUser(ctx, 8, "dave", "", "")
	f.st.CreateTeam(ctx, "ops", "Ops", dave.ID)

	_, body, _ := f.owner.get("/admin")
	mustContain(t, "admin", body, `id="team-web"`, `id="team-ops"`, `role="search"`, "Bir limitte")
	_, body, _ = f.owner.get("/admin?q=dave")
	mustContain(t, "search", body, `id="team-ops"`, "Filtreyi temizle")
	mustNotContain(t, "search", body, `id="team-web"`)
	_, body, _ = f.owner.get("/admin?q=nothing")
	mustContain(t, "no match", body, "Aramana uyan ekip yok")

	if code, _, _ := form(f.owner, "/admin", "/admin/teams/web/plan", url.Values{"plan": {"pro"}, "limits": {"projects=50"}}); code != http.StatusSeeOther {
		t.Fatalf("limits: %d", code)
	}
	if code, _, _ := form(f.owner, "/admin", "/admin/teams/web/plan", url.Values{"plan": {"hobby"}}); code != http.StatusSeeOther {
		t.Fatalf("inline plan: %d", code)
	}
	tp, _ := f.st.GetTeamPlan(ctx, f.teamID(t))
	if tp.Plan != store.PlanHobby || !strings.Contains(string(tp.Overrides), `"projects": 50`) {
		t.Fatalf("inline plan lost overrides: %+v %s", tp, tp.Overrides)
	}

	code, body, _ := form(f.owner, "/admin", "/admin/teams/web/suspend", url.Values{"reason": {"  "}})
	if code != http.StatusBadRequest {
		t.Fatalf("suspend without reason: %d", code)
	}
	mustContain(t, "no reason", body, "Askıya alma gerekçesi yaz")
	form(f.owner, "/admin", "/admin/teams/web/suspend", url.Values{"reason": {"spam"}})
	_, body, _ = f.owner.get("/admin?filter=suspended")
	mustContain(t, "suspended filter", body, `id="team-web"`, "Gerekçe: spam", "Askıyı kaldır")
	mustNotContain(t, "suspended filter", body, `id="team-ops"`)
	_, body, _ = f.viewer.get("/teams/web")
	mustContain(t, "suspended team page", body, "askıya alındı: spam")
}

// Notifications: the guided channel form, grouped events with every type,
// the viewer's reason.
func TestNotificationsPageUX(t *testing.T) {
	f := planUI(t, nil)
	_, body, _ := f.owner.get("/teams/web/notifications")
	mustContain(t, "notifications", body, `id="k-slack" name="kind" value="slack" checked`, `id="k-email"`,
		"Incoming Webhooks", "Yeni Webhook", "Deploy&#39;lar", "Altyapı", "Production", `aria-current="page" href="/teams/web/notifications"`)
	for _, e := range notify.EventTypes {
		if !strings.Contains(body, `name="events" value="`+e+`"`) {
			t.Errorf("event %s missing from the grouped form", e)
		}
	}
	_, body, _ = f.viewer.get("/teams/web/notifications")
	mustContain(t, "viewer", body, "Kanalları yönetmek için ekipte en az Üye rolü gerekir", "E-posta tercihlerim")
	mustNotContain(t, "viewer", body, `id="new-channel"`)

	// A created channel shows its card and last delivery after a test.
	csrf := f.owner.csrf("/teams/web/notifications")
	f.owner.post("/teams/web/notifications", url.Values{"csrf": {csrf}, "name": {"ops"}, "kind": {"slack"},
		"url": {"https://hooks.slack.com/services/T/B/x"}, "events": {notify.DeploymentFailed}}, "self")
	chs, _ := f.st.ListNotificationChannels(context.Background(), f.teamID(t))
	if len(chs) != 1 {
		t.Fatalf("channels: %d", len(chs))
	}
	_, body, _ = f.owner.get("/teams/web/notifications")
	mustContain(t, "channel card", body, "Henüz gönderim yok", "Test gönder", "Kanalı sil", `hx-confirm="&quot;ops&quot; kanalı silinsin mi?`)
	f.owner.post(fmt.Sprintf("/teams/web/notifications/%d/test", chs[0].ID), url.Values{"csrf": {csrf}}, "self")
	_, body, _ = f.owner.get("/teams/web/notifications")
	mustContain(t, "after test", body, "Son gönderim:")
}

// The protected deploy's login page explains itself and offers team sign-in.
func TestProtectLoginUX(t *testing.T) {
	f, _ := setupProtect(t)
	protect.Save(context.Background(), f.st, f.app, store.ProtectPassword, "", "open sesame!")
	login := "/_protect/login?" + url.Values{"app": {"blog"}, "next": {"http://feature-blog.paas.test/"}}.Encode()
	_, body, _ := f.ui.get(login)
	mustContain(t, "protect login", body, "Bu deploy korumalı", "şifreyle korumaya aldı", "Ekip hesabınla giriş yap", `href="/login?next=`)
}

// Legal pages have a table of contents linking to their sections.
func TestLegalTOC(t *testing.T) {
	f := planUI(t, nil)
	_, body, _ := f.ui.get("/terms")
	mustContain(t, "terms", body, `aria-label="İçindekiler"`, `href="#iletisim"`, `id="iletisim"`, `href="/privacy"`)
	_, body, _ = f.ui.get("/privacy")
	mustContain(t, "privacy", body, `href="#islenen-veriler"`, `id="islenen-veriler"`)
}
