package web_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/store"
)

// Faz 28: Ayarlar › Tehlikeli bölge › Projeyi sil. Owners get the typed-name
// form, members and viewers a disabled button with the reason; the server
// checks CSRF, the role and the name; success lands on the dashboard with
// the flash and the project is gone.
func TestDeleteProjectForm(t *testing.T) {
	f := setupF20(t, nil)
	ctx := context.Background()
	bob, _ := f.st.UpsertUser(ctx, 3, "bob", "", "")
	team, _ := f.st.GetTeamBySlug(ctx, "web")
	f.st.SetMember(ctx, team.ID, bob.ID, store.RoleMember)
	member := f.as(bob.ID)

	_, body, _ := f.owner.get("/apps/blog/settings")
	mustContain(t, "owner settings", body, `action="/apps/blog/delete"`, `name="confirm"`, `data-delete-name="blog"`,
		"Projeyi kalıcı olarak sil", "Bu işlem geri alınamaz", "Tüm deploy'lar", "Bağlı alan adları",
		"Veritabanları ve içindeki veriler", "Ortam değişkenleri", `for="delete-confirm"`, "input[data-delete-name]")
	mustNotContain(t, "owner settings", body, "henüz yok", "platform yöneticisine yaz")
	for who, c := range map[string]*ui{"viewer": f.viewer, "member": member} {
		_, body, _ := c.get("/apps/blog/settings")
		mustContain(t, who+" settings", body, "Projeyi yalnızca ekip sahipleri silebilir",
			`<button type="button" class="danger" disabled aria-describedby="delete-owner">Projeyi sil</button>`)
		mustNotContain(t, who+" settings", body, `action="/apps/blog/delete"`)
	}

	// CSRF, Origin and roles.
	f.formChecks("/apps/blog/delete", url.Values{"confirm": {"blog"}})
	if code, body, _ := form(member, "/apps/blog/settings", "/apps/blog/delete", url.Values{"confirm": {"blog"}}); code != http.StatusForbidden ||
		!strings.Contains(body, "Projeyi yalnızca ekip sahipleri silebilir") {
		t.Fatalf("member: %d", code)
	}
	// A wrong name is refused on the settings page.
	code, body, _ := form(f.owner, "/apps/blog/settings", "/apps/blog/delete", url.Values{"confirm": {"Blog"}})
	if code != http.StatusBadRequest {
		t.Fatalf("wrong name: %d", code)
	}
	mustContain(t, "wrong name", body, "Onaylamak için proje adını tam olarak yaz: blog", `aria-invalid="true"`)
	if _, err := f.st.GetAppByName(ctx, "blog"); err != nil {
		t.Fatalf("refused deletion changed the app: %v", err)
	}

	// The right name deletes and redirects to the dashboard with a flash.
	code, _, h := form(f.owner, "/apps/blog/settings", "/apps/blog/delete", url.Values{"confirm": {" blog "}})
	if code != http.StatusSeeOther || h.Get("Location") != "/?ok=deleted&app=blog" {
		t.Fatalf("delete: %d %q", code, h.Get("Location"))
	}
	_, body, _ = f.owner.get(h.Get("Location"))
	mustContain(t, "dashboard", body, "„blog“ projesi silindi.")
	mustNotContain(t, "dashboard", body, `href="/apps/blog"`)
	if code, _, _ := f.owner.get("/apps/blog"); code != http.StatusNotFound {
		t.Fatalf("project page after delete: %d", code)
	}
	if _, err := f.st.GetAppByID(ctx, f.app.ID); err == nil {
		t.Fatal("app row survived")
	}
	// The flash only echoes valid names.
	_, body, _ = f.owner.get("/?ok=deleted&app=%3Cb%3Ex")
	mustNotContain(t, "bad flash", body, "projesi silindi", "<b>x")

	// The name is free again; an htmx form gets HX-Redirect.
	if _, err := f.st.CreateAppInTeam(ctx, team.ID, "blog", "nisagwn/blog", "main"); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	v := url.Values{"confirm": {"blog"}, "csrf": {f.owner.csrf("/apps/blog/settings")}}
	req, _ := http.NewRequest("POST", f.srv0()+"/apps/blog/delete", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", f.srv0())
	req.Header.Set("HX-Request", "true")
	resp, err := f.owner.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("HX-Redirect") != "/?ok=deleted&app=blog" {
		t.Fatalf("htmx delete: %d %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
}

// A deletion that is still running in the background shows its own
// variant of the flash.
func TestDeleteProjectFlashVariants(t *testing.T) {
	f := setupF20(t, nil)
	_, body, _ := f.owner.get("/?ok=deleting&app=blog")
	mustContain(t, "deleting flash", body, "„blog“ projesi silindi. Kalan kaynaklar arka planda kaldırılıyor")
}
