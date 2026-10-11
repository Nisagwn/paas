package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/store"
)

// Project and deployment screens: the production hero of Genel, the deploy
// list filters, the deployment timeline and failure explanation, and the
// bulk .env editor.

// failed adds a deployment of branch that built and failed with reason.
func (f *f20) failed(n int, branch, msg, reason string, logs ...string) store.Deployment {
	f.t.Helper()
	ctx := context.Background()
	d, _, err := f.st.EnqueueDeployment(ctx, f.app.ID, sha(n), branch, msg)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.st.ClaimNext(ctx); err != nil {
		f.t.Fatal(err)
	}
	for _, l := range logs {
		f.st.AppendLog(ctx, d.ID, l)
	}
	if err := f.st.MarkFailed(ctx, d.ID, reason); err != nil {
		f.t.Fatal(err)
	}
	d, _ = f.st.GetDeployment(ctx, d.ID)
	return d
}

func (f *f20) ready(n int, branch, msg string, aliases ...store.AliasSpec) store.Deployment {
	f.t.Helper()
	ctx := context.Background()
	d, _, err := f.st.EnqueueDeployment(ctx, f.app.ID, sha(n), branch, msg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.st.SetImage(ctx, d.ID, fmt.Sprintf("registry.test/blog@sha256:%d", n))
	if err := f.st.MarkReady(ctx, d, aliases); err != nil {
		f.t.Fatal(err)
	}
	d, _ = f.st.GetDeployment(ctx, d.ID)
	return d
}

var prodAlias = store.AliasSpec{Hostname: "blog.paas.test", Kind: store.AliasProduction, Branch: "main"}

func TestOverviewHero(t *testing.T) {
	f := setupF20(t, nil)

	// Before the first deployment: the next step, no actions.
	_, body, _ := f.owner.get("/apps/blog")
	mustContain(t, "empty overview", body, "Henüz yayında değil", "branch'ine push ettiğinde ilk production deploy'u başlar")
	mustNotContain(t, "empty overview", body, "Geri al")

	d1 := f.ready(1, "main", "ilk sürüm", prodAlias)
	_, body, _ = f.owner.get("/apps/blog")
	mustContain(t, "one production", body, `aria-labelledby="prod-h"`, "https://blog.paas.test", `data-copy="https://blog.paas.test"`,
		"ilk sürüm", "Siteyi aç ↗", "Yeniden deploy et", `name="from" value="app"`,
		// Nothing to roll back to yet: disabled, with the reason on screen.
		"Geri dönülecek, hâlâ çalışan önceki bir production deploy&#39;u yok.",
		"Trafik yok", // health at a glance
		`href="/apps/blog/settings#processes"`, `href="/apps/blog/settings#rollout"`, `href="/apps/blog/databases"`,
		"Önizlemeler", "dışındaki bir branch'e push ettiğinde")

	d2 := f.ready(2, "main", "ikinci sürüm", prodAlias)
	f.ready(3, "feature", "önizleme işi", store.AliasSpec{Hostname: "feature-blog.paas.test", Kind: store.AliasPreview, Branch: "feature"})
	f.failed(4, "main", "bozuk sürüm", "build: buildctl: exit status 1")
	_, body, _ = f.owner.get("/apps/blog")
	mustContain(t, "overview", body,
		// The failed push is flagged, production is not.
		"Son deploy başarısız oldu", "Canlı site etkilenmedi", fmt.Sprintf(`href="/deployments/%d"`, d2.ID),
		// Rollback goes to the previous production deployment.
		fmt.Sprintf(`name="deployment_id" value="%d"`, d1.ID), "Önceki sürüme döner",
		"feature-blog.paas.test", "Build komutu başarısız oldu")
	mustNotContain(t, "overview", body, "Şu an süren bir yayın yok", "Terminal oturumları")

	// A viewer sees the same hero with disabled actions and the reason.
	_, body, _ = f.viewer.get("/apps/blog")
	mustContain(t, "viewer overview", body, "Bu işlem için ekipte üye rolü gerekir.", "Siteyi aç ↗")
	mustNotContain(t, "viewer overview", body, `action="/apps/blog/redeploy"`, `action="/apps/blog/rollback"`)

	// "Geri al" from Genel returns to Genel with a message.
	v := url.Values{"csrf": {f.owner.csrf("/apps/blog")}, "deployment_id": {fmt.Sprint(d1.ID)}, "from": {"app"}}
	code, _, h := f.owner.post("/apps/blog/rollback", v, "self")
	if code != http.StatusSeeOther || h.Get("Location") != "/apps/blog?ok=rollback" {
		t.Fatalf("rollback from Genel: %d %s", code, h.Get("Location"))
	}
	_, body, _ = f.owner.get("/apps/blog?ok=rollback")
	mustContain(t, "after rollback", body, `class="flash ok" role="status"`, "Canlı site artık seçtiğin sürümü gösteriyor.")
}

func TestDeployListFilters(t *testing.T) {
	f := setupF20(t, nil)
	f.ready(1, "main", "canlı sürüm", prodAlias)
	f.ready(2, "feature", "önizleme sürümü", store.AliasSpec{Hostname: "feature-blog.paas.test", Kind: store.AliasPreview, Branch: "feature"})
	f.failed(3, "fix", "kırık düzeltme", "build: buildctl: exit status 1")

	_, body, _ := f.owner.get("/apps/blog/deployments")
	mustContain(t, "all", body, "canlı sürüm", "önizleme sürümü", "kırık düzeltme", "Son 3 deploy.",
		`<option value="feature">feature</option>`, `id="f-env"`, `id="f-status"`, `id="f-branch"`, "Ayrıntılar ve log")

	_, body, _ = f.owner.get("/apps/blog/deployments?env=preview")
	mustContain(t, "preview", body, "önizleme sürümü", "kırık düzeltme", "2 / 3 deploy gösteriliyor", "Filtreleri temizle",
		`<option value="preview" selected>`)
	mustNotContain(t, "preview", body, "canlı sürüm")

	_, body, _ = f.owner.get("/apps/blog/deployments?status=failed&branch=fix")
	mustContain(t, "failed", body, "kırık düzeltme", "Build komutu başarısız oldu")
	mustNotContain(t, "failed", body, "önizleme sürümü", "canlı sürüm")

	_, body, _ = f.owner.get("/apps/blog/deployments?status=active")
	mustContain(t, "none", body, "Bu filtreye uyan deploy yok.")

	// Unknown values are ignored.
	_, body, _ = f.owner.get("/apps/blog/deployments?env=x&status=y&branch=nope")
	mustContain(t, "unknown filter", body, "Son 3 deploy.")

	// While something runs, the polled fragment keeps the filter.
	f.st.EnqueueDeployment(context.Background(), f.app.ID, sha(9), "feature", "sürüyor")
	req, _ := http.NewRequest("GET", f.srv0()+"/apps/blog/deployments?env=preview", nil)
	req.Header.Set("HX-Request", "true")
	resp, err := f.owner.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	mustContain(t, "fragment", string(b), `hx-get="/apps/blog/deployments?env=preview"`, "sürüyor")
	mustNotContain(t, "fragment", string(b), "canlı sürüm")
}

func TestDeploymentPageUX(t *testing.T) {
	f := setupF20(t, nil)
	ready := f.ready(1, "main", "canlı sürüm", prodAlias)
	bad := f.failed(2, "main", "kırık build\n\nayrıntı", "build: buildctl: exit status 1",
		"==> building registry/blog", "#12 [build 5/6] RUN npm run build",
		"#12 ERROR: process \"/bin/sh -c npm run build\" did not complete successfully: exit code: 1")

	_, body, _ := f.owner.get(fmt.Sprintf("/deployments/%d", bad.ID))
	mustContain(t, "failed deployment", body,
		"<h1>kırık build</h1>", `aria-label="Deploy adımları"`,
		`<li class="done" data-step="queued">`, `<li class="failed" data-step="build">`, `<li class="pending" data-step="deploy">`,
		"Build komutu başarısız oldu", "Ne yapmalısın:", `href="/apps/blog/settings#build"`,
		"#12 ERROR: process", `data-jump="error"`, "Teknik hata mesajı", "build: buildctl: exit status 1",
		`id="follow"`, "Takip et", `id="to-error"`,
		// Actions stay visible; unavailable ones say why.
		"Başarısız deploy&#39;un çalışan bir kopyası yok", ">Yeniden deploy et<", `name="from" value="deployment"`)

	_, body, _ = f.owner.get(fmt.Sprintf("/deployments/%d", ready.ID))
	mustContain(t, "live deployment", body, `class="tag prod"`, "Bu deploy zaten canlı.", `href="/deployments/`+fmt.Sprint(ready.ID)+`/review"`)
	mustNotContain(t, "live deployment", body, `class="fail`)

	// Viewers see every action disabled with the role reason.
	_, body, _ = f.viewer.get(fmt.Sprintf("/deployments/%d", bad.ID))
	mustContain(t, "viewer", body, "Bu işlem için ekipte üye rolü gerekir.")
	mustNotContain(t, "viewer", body, `action="/apps/blog/redeploy"`, `action="/apps/blog/rollback"`)

	// A rollback from the deployment page comes back to it.
	f.ready(3, "main", "yeni canlı", prodAlias) // production moves on
	v := url.Values{"csrf": {f.owner.csrf("/apps/blog")}, "deployment_id": {fmt.Sprint(ready.ID)}, "from": {"deployment"}}
	code, _, h := f.owner.post("/apps/blog/rollback", v, "self")
	if want := fmt.Sprintf("/deployments/%d?ok=rollback-here", ready.ID); code != http.StatusSeeOther || h.Get("Location") != want {
		t.Fatalf("rollback from deployment page: %d %s, want %s", code, h.Get("Location"), want)
	}
	_, body, _ = f.owner.get(fmt.Sprintf("/deployments/%d?ok=rollback-here", ready.ID))
	mustContain(t, "after rollback", body, "Canlı site artık bu deploy&#39;u gösteriyor.", "Bu deploy zaten canlı.")
}

func TestEnvBulk(t *testing.T) {
	f := setupF20(t, nil)
	ctx := context.Background()
	csrf := f.owner.csrf("/apps/blog/settings")
	_, body, _ := f.owner.get("/apps/blog/settings")
	mustContain(t, "settings", body, `aria-label="Ayar bölümleri"`, `href="#danger"`, "Tehlikeli bölge", `id="general"`,
		`id="env-dotenv"`, `data-reveal="env-value"`, `href="/teams/web/notifications"`, `id="processes"`, `id="rollout"`)

	form := url.Values{"csrf": {csrf}, "target": {"preview"}, "git_branch": {"feature/x"},
		"dotenv": {"# yorum\nAPI_KEY=abc\nexport DB_URL=\"postgres://u:p@h/db\"\n"}}
	code, _, h := f.owner.post("/apps/blog/env/bulk", form, "self")
	if code != http.StatusSeeOther || h.Get("Location") != "/apps/blog/settings?ok=env-bulk#env" {
		t.Fatalf("bulk: %d %s", code, h.Get("Location"))
	}
	vars, _ := f.st.ListEnv(ctx, f.app.ID)
	if len(vars) != 2 || vars[0].Key != "API_KEY" || vars[0].Target != store.EnvPreview || vars[0].GitBranch != "feature/x" {
		t.Fatalf("vars: %+v", vars)
	}

	// A bad line names the line and keeps the text; nothing is saved.
	form = url.Values{"csrf": {csrf}, "target": {"all"}, "dotenv": {"OK=1\nPORT=80\n"}}
	code, body, _ = f.owner.post("/apps/blog/env/bulk", form, "self")
	if code != http.StatusBadRequest {
		t.Fatalf("bad bulk: %d", code)
	}
	mustContain(t, "bad bulk", body, "2. satır: PORT platform tarafından ayarlanır.", "OK=1\nPORT=80")
	if vars, _ := f.st.ListEnv(ctx, f.app.ID); len(vars) != 2 {
		t.Fatalf("a refused paste saved %d vars", len(vars))
	}

	// htmx gets the card back with a success message.
	form = url.Values{"csrf": {csrf}, "target": {"production"}, "dotenv": {"A=1\nB=2\nC=3"}}
	req, _ := http.NewRequest("POST", f.srv0()+"/apps/blog/env/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Origin", f.srv0())
	resp, err := f.owner.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	mustContain(t, "htmx bulk", string(b), `<div id="env-card"`, "3 değişken kaydedildi")

	f.formChecks("/apps/blog/env/bulk", url.Values{"dotenv": {"X=1"}})
	if vars, _ := f.st.ListEnv(ctx, f.app.ID); len(vars) != 5 {
		t.Fatalf("rejected posts changed vars: %d", len(vars))
	}
}
