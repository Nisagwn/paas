package web_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
)

// TestNotificationsPage: the team's Bildirimler page — roles, CSRF and
// Origin checks, secrets shown once, validation in Turkish, "Test gönder",
// email preferences and deletion.
func TestNotificationsPage(t *testing.T) {
	u := setup(t)
	ctx := context.Background()
	st := u.st
	alice, _ := st.UpsertUser(ctx, 1, "alice", "", "")
	bob, _ := st.UpsertUser(ctx, 2, "bob", "", "")
	carol, _ := st.UpsertUser(ctx, 3, "carol", "", "")
	dave, _ := st.UpsertUser(ctx, 4, "dave", "", "")
	team, _ := st.CreateTeam(ctx, "web", "Web", alice.ID)
	st.SetMember(ctx, team.ID, bob.ID, store.RoleMember)
	st.SetMember(ctx, team.ID, carol.ID, store.RoleViewer)
	st.CreateTeam(ctx, "ops", "Ops", dave.ID)
	st.CreateAppInTeam(ctx, team.ID, "blog", "nisagwn/blog", "main")
	member, viewer, other := u.as(bob.ID), u.as(carol.ID), u.as(dave.ID)
	page := "/teams/web/notifications"

	if _, body, _ := u.as(alice.ID).get("/teams/web"); !strings.Contains(body, `href="/teams/web/notifications"`) {
		t.Fatal("team page lacks the Bildirimler link")
	}
	if code, _, _ := other.get(page); code != http.StatusNotFound {
		t.Fatalf("other team: %d", code)
	}
	code, body, _ := viewer.get(page)
	if code != 200 || !strings.Contains(body, "Henüz bildirim kanalı yok") || strings.Contains(body, "Kanal ekle") {
		t.Fatalf("viewer page: %d\n%s", code, body)
	}

	csrf := member.csrf(page)
	hook := "https://hooks.slack.com/services/T000/B000/topsecret"
	form := url.Values{"csrf": {csrf}, "name": {"#deploys"}, "kind": {"slack"}, "url": {hook},
		"events": {notify.DeploymentFailed, notify.ProductionChanged}, "apps": {"blog"}}
	// A cross-origin request and a wrong token are refused.
	if code, _, _ := member.post(page, form, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin create: %d", code)
	}
	bad := url.Values{}
	for k, v := range form {
		bad[k] = v
	}
	bad.Set("csrf", "wrong")
	if code, _, _ := member.post(page, bad, "self"); code != http.StatusForbidden {
		t.Fatalf("wrong CSRF token: %d", code)
	}
	vform := url.Values{}
	for k, v := range form {
		vform[k] = v
	}
	vform.Set("csrf", viewer.csrf(page))
	if code, _, _ := viewer.post(page, vform, "self"); code != http.StatusForbidden {
		t.Fatalf("viewer create: %d", code)
	}
	if chs, _ := st.ListNotificationChannels(ctx, team.ID); len(chs) != 0 {
		t.Fatalf("refused requests created %d channels", len(chs))
	}

	if code, _, h := member.post(page, form, "self"); code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "ok=notify-created") {
		t.Fatalf("create: %d %v", code, h)
	}
	_, body, _ = member.get(page + "?ok=notify-created")
	for _, want := range []string{"Bildirim kanalı eklendi.", "#deploys", "https://hooks.slack.com/…", "Test gönder", "Kanalı sil", "Deploy başarısız"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "topsecret") {
		t.Fatal("page shows the webhook URL")
	}

	// Invalid input is shown in Turkish and the form keeps its values.
	bad = url.Values{"csrf": {csrf}, "name": {"kanal"}, "kind": {"discord"}, "url": {"http://10.0.0.1/hook"}, "events": {notify.DeploymentFailed}}
	code, body, _ = member.post(page, bad, "self")
	if code != http.StatusBadRequest || !strings.Contains(body, "URL https:// ile başlayan tam bir adres olmalı.") || !strings.Contains(body, `value="kanal"`) {
		t.Fatalf("invalid create: %d\n%s", code, body)
	}

	// A webhook channel shows its signing secret once.
	code, body, _ = member.post(page, url.Values{"csrf": {csrf}, "name": {"ci"}, "kind": {"webhook"},
		"url": {"https://ci.example.com/paas"}, "events": {notify.DeploymentReady}}, "self")
	if code != http.StatusCreated || !strings.Contains(body, "whsec_") || !strings.Contains(body, "X-Paas-Signature") {
		t.Fatalf("webhook create: %d\n%s", code, body)
	}
	if _, body, _ = member.get(page); strings.Contains(body, "whsec_") {
		t.Fatal("signing secret shown again")
	}

	chs, _ := st.ListNotificationChannels(ctx, team.ID)
	var slack store.NotificationChannel
	for _, c := range chs {
		if c.Name == "#deploys" {
			slack = c
		}
	}
	if slack.ID == 0 || len(slack.Apps) != 1 || slack.Apps[0] != "blog" || len(slack.Events) != 2 {
		t.Fatalf("stored channel: %+v", slack)
	}
	chPath := fmt.Sprintf("%s/%d", page, slack.ID)

	// Update: turn it off and add an event; the URL stays.
	if code, _, _ := member.post(chPath, url.Values{"csrf": {csrf}, "name": {"#deploys"},
		"events": {notify.DeploymentFailed, notify.DeploymentReady}}, "self"); code != http.StatusSeeOther {
		t.Fatalf("update: %d", code)
	}
	slack, _ = st.GetNotificationChannel(ctx, team.ID, slack.ID)
	if slack.Enabled || len(slack.Events) != 2 || slack.Events[0] != notify.DeploymentReady {
		t.Fatalf("updated channel: %+v", slack)
	}
	if sec, _ := st.NotificationChannelSecrets(ctx, slack.ID); sec.URL != hook {
		t.Fatalf("URL changed: %q", sec.URL)
	}

	// Test gönder: queued (no dispatcher in this server) and in the log.
	if code, _, _ := member.post(chPath+"/test", url.Values{"csrf": {csrf}}, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin test: %d", code)
	}
	if code, _, h := member.post(chPath+"/test", url.Values{"csrf": {csrf}}, "self"); code != http.StatusSeeOther ||
		!strings.Contains(h.Get("Location"), "ok=notify-test-queued") {
		t.Fatalf("test: %d %v", code, h)
	}
	if _, body, _ = viewer.get(page); !strings.Contains(body, "Test bildirimi") || !strings.Contains(body, "Son gönderimler") {
		t.Fatalf("delivery log:\n%s", body)
	}

	// Email preferences: carol opts out of everything but failures.
	vcsrf := viewer.csrf(page)
	if code, _, _ := viewer.post(page+"/preferences", url.Values{"csrf": {vcsrf}, "email": {"carol@example.com"},
		"email_enabled": {"1"}, "events": {notify.DeploymentFailed}}, "self"); code != http.StatusSeeOther {
		t.Fatalf("prefs: %d", code)
	}
	p, _ := st.GetNotificationPrefs(ctx, carol.ID)
	if p.Email != "carol@example.com" || !p.EmailEnabled || !p.Wants(notify.DeploymentFailed) || p.Wants(notify.DeploymentReady) {
		t.Fatalf("prefs: %+v", p)
	}
	if code, body, _ := viewer.post(page+"/preferences", url.Values{"csrf": {vcsrf}, "email": {"not an address"}}, "self"); code != http.StatusBadRequest ||
		!strings.Contains(body, "Geçersiz e-posta adresi") {
		t.Fatalf("bad prefs: %d", code)
	}

	// Delete: viewer refused, member allowed.
	if code, _, _ := viewer.post(chPath+"/delete", url.Values{"csrf": {vcsrf}}, "self"); code != http.StatusForbidden {
		t.Fatalf("viewer delete: %d", code)
	}
	if code, _, _ := member.post(chPath+"/delete", url.Values{"csrf": {csrf}}, "self"); code != http.StatusSeeOther {
		t.Fatalf("delete: %d", code)
	}
	if _, err := st.GetNotificationChannel(ctx, team.ID, slack.ID); err == nil {
		t.Fatal("channel not deleted")
	}
}
