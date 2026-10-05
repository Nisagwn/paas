package web_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
)

// as returns a client signed in as uid (session cookie issued directly).
func (u *ui) as(uid int64) *ui {
	rec := httptest.NewRecorder()
	auth.New(token).IssueUser(rec, httptest.NewRequest("GET", "/", nil), uid)
	c := newClient()
	su, _ := url.Parse(u.srv.URL)
	c.Jar.SetCookies(su, rec.Result().Cookies())
	return &ui{t: u.t, st: u.st, srv: u.srv, client: c}
}

func TestPageAuthorization(t *testing.T) {
	u := setup(t)
	ctx := context.Background()
	st := u.st
	alice, _ := st.UpsertUser(ctx, 1, "alice", "", "")
	carol, _ := st.UpsertUser(ctx, 2, "carol", "", "")
	dave, _ := st.UpsertUser(ctx, 3, "dave", "", "")
	web, _ := st.CreateTeam(ctx, "web", "Web", alice.ID)
	st.SetMember(ctx, web.ID, carol.ID, store.RoleViewer)
	st.CreateTeam(ctx, "ops", "Ops", dave.ID)
	app, _ := st.CreateAppInTeam(ctx, web.ID, "blog", "nisagwn/blog", "main")
	prod := []store.AliasSpec{{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}}
	d1, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "one")
	d2, _, _ := st.EnqueueDeployment(ctx, app.ID, sha(2), "main", "two")
	st.MarkReady(ctx, d1, prod)
	st.MarkReady(ctx, d2, prod)
	dep := fmt.Sprintf("/deployments/%d", d1.ID)

	owner, viewer, other := u.as(alice.ID), u.as(carol.ID), u.as(dave.ID)
	for _, c := range []struct {
		who  *ui
		name string
		path string
		want int
	}{
		{owner, "owner", "/apps/blog", 200}, {viewer, "viewer", "/apps/blog", 200}, {other, "other", "/apps/blog", 404},
		{owner, "owner", dep, 200}, {viewer, "viewer", dep, 200}, {other, "other", dep, 404},
		{viewer, "viewer", "/teams/web", 200}, {other, "other", "/teams/web", 404},
	} {
		if code, _, _ := c.who.get(c.path); code != c.want {
			t.Errorf("GET %s as %s: %d, want %d", c.path, c.name, code, c.want)
		}
	}
	// Listing shows only the caller's teams' apps.
	if _, body, _ := other.get("/"); strings.Contains(body, "/apps/blog") {
		t.Error("other team's app listed")
	}
	// Viewers see no write controls, and posting anyway is refused.
	if _, body, _ := viewer.get("/apps/blog"); strings.Contains(body, "Bu sürüme dön") || strings.Contains(body, `action="/apps/blog/env"`) {
		t.Error("viewer sees write controls")
	}
	csrf := viewer.csrf("/apps/blog")
	rb := url.Values{"deployment_id": {fmt.Sprint(d1.ID)}, "csrf": {csrf}}
	if code, _, _ := viewer.post("/apps/blog/rollback", rb, "self"); code != http.StatusForbidden {
		t.Errorf("viewer rollback: %d, want 403", code)
	}
	if code, _, _ := viewer.post("/teams/web/members", url.Values{"login": {"dave"}, "role": {"owner"}, "csrf": {csrf}}, "self"); code != http.StatusForbidden {
		t.Errorf("viewer adds member: %d, want 403", code)
	}
	if code, _, _ := other.post("/apps/blog/rollback", url.Values{"deployment_id": {fmt.Sprint(d1.ID)}, "csrf": {other.csrf("/")}}, "self"); code != http.StatusNotFound {
		t.Errorf("other rollback: %d, want 404", code)
	}
	ocsrf := owner.csrf("/apps/blog")
	if code, _, _ := owner.post("/apps/blog/rollback", url.Values{"deployment_id": {fmt.Sprint(d1.ID)}, "csrf": {ocsrf}}, "self"); code != http.StatusSeeOther {
		t.Errorf("owner rollback: %d", code)
	}

	// Tokens page: create shows the token once; revoke removes it.
	code, body, _ := owner.post("/tokens", url.Values{"name": {"laptop"}, "csrf": {ocsrf}}, "self")
	if code != http.StatusCreated || !strings.Contains(body, "paas_") {
		t.Fatalf("create token: %d", code)
	}
	if _, body, _ := owner.get("/tokens"); !strings.Contains(body, "laptop") || strings.Contains(body, "<code>paas_") {
		t.Fatal("token listing must show the name, never the token")
	}
	toks, _ := st.APITokens(ctx, alice.ID)
	if code, _, _ := viewer.post("/tokens/revoke", url.Values{"id": {fmt.Sprint(toks[0].ID)}, "csrf": {csrf}}, "self"); code != http.StatusNotFound {
		t.Errorf("revoke someone else's token: %d", code)
	}
	if code, _, _ := owner.post("/tokens/revoke", url.Values{"id": {fmt.Sprint(toks[0].ID)}, "csrf": {ocsrf}}, "self"); code != http.StatusSeeOther {
		t.Errorf("revoke: %d", code)
	}

	// Team creation through the UI.
	if code, _, h := other.post("/teams", url.Values{"slug": {"data"}, "name": {"Data"}, "csrf": {other.csrf("/teams")}}, "self"); code != http.StatusSeeOther ||
		h.Get("Location") != "/teams/data?ok=team" {
		t.Errorf("create team: %d %s", code, h.Get("Location"))
	}
}
