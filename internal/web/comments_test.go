package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/web"
)

type webSync struct {
	mu      sync.Mutex
	changed []int64
}

func (w *webSync) CommentChanged(_, threadID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.changed = append(w.changed, threadID)
}

func (w *webSync) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.changed)
}

func (f *f20) ownerID(t *testing.T) int64 {
	u, err := f.st.UserByLogin(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func fmtCopy(b *strings.Builder, resp *http.Response) {
	defer resp.Body.Close()
	io.Copy(b, resp.Body)
}

// Faz 25: the review page, its CSP, the comment forms and their checks.
func TestReviewPage(t *testing.T) {
	sync := &webSync{}
	f := setupF20(t, func(s *web.Server) { s.Comments = sync })
	ctx := context.Background()
	d, _, _ := f.st.EnqueueDeployment(ctx, f.app.ID, sha(1), "feature", "yeni tasarım")
	f.st.MarkReady(ctx, d, nil)
	queued, _, _ := f.st.EnqueueDeployment(ctx, f.app.ID, sha(2), "feature", "wip")
	review := fmt.Sprintf("/deployments/%d/review", d.ID)

	// The page: iframe of the deployment, sandboxed, and frame-src for the
	// app hosts on this page only.
	code, body, h := f.viewer.get(review + "?path=/fiyat")
	if code != 200 {
		t.Fatalf("review page: %d\n%s", code, body)
	}
	host := d.Host("paas.test")
	mustContain(t, "review page", body, `src="https://`+host+`/fiyat"`,
		`sandbox="allow-scripts allow-same-origin allow-forms"`, `id="overlay"`, "Yorum modu", `action="/deployments/`)
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src https://*.paas.test") ||
		!strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("review CSP = %q", csp)
	}
	if _, _, h := f.viewer.get("/apps/blog"); strings.Contains(h.Get("Content-Security-Policy"), "frame-src") {
		t.Fatal("frame-src leaked to other pages")
	}
	// A deployment that is not ready shows no iframe; other teams get 404.
	if _, body, _ := f.owner.get(fmt.Sprintf("/deployments/%d/review", queued.ID)); strings.Contains(body, "<iframe") ||
		!strings.Contains(body, "hazır değil") {
		t.Fatal("queued deployment embedded")
	}
	if code, _, _ := f.get(review); code != http.StatusSeeOther {
		t.Fatalf("anonymous review: %d", code)
	}
	ctxStranger, _ := f.st.UpsertUser(ctx, 3, "dave", "", "")
	f.st.CreateTeam(ctx, "ops", "Ops", ctxStranger.ID)
	if code, _, _ := f.as(ctxStranger.ID).get(review); code != http.StatusNotFound {
		t.Fatalf("other team's review: %d", code)
	}

	// New thread: CSRF and Origin are checked; a viewer may comment.
	post := fmt.Sprintf("/deployments/%d/comments", d.ID)
	form := url.Values{"body": {"Başlık taşıyor @alice"}, "path": {"/fiyat"}, "x": {"25.5"}, "y": {"40"}, "viewport_width": {"1280"}}
	if code, _, _ := f.viewer.post(post, form, "self"); code != http.StatusForbidden {
		t.Fatalf("no CSRF: %d", code)
	}
	form.Set("csrf", f.viewer.csrf(review))
	if code, _, _ := f.viewer.post(post, form, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", code)
	}
	if sync.count() != 0 {
		t.Fatal("rejected forms synced")
	}
	code, _, h = f.viewer.post(post, form, "self")
	if code != http.StatusSeeOther || !strings.HasPrefix(h.Get("Location"), review+"?path=%2Ffiyat&thread=") {
		t.Fatalf("create thread: %d → %q", code, h.Get("Location"))
	}
	threads, _ := f.st.ListThreads(ctx, d.ID, "")
	if len(threads) != 1 || threads[0].AuthorLogin != "carol" || *threads[0].X != 25.5 || strings.Join(threads[0].Mentions, ",") != "alice" {
		t.Fatalf("stored thread: %+v", threads)
	}
	th := threads[0]
	if sync.count() != 1 {
		t.Fatalf("sync calls: %d", sync.count())
	}

	// Validation errors are Turkish and keep the draft.
	bad := url.Values{"body": {"x"}, "path": {"//evil"}, "csrf": form["csrf"]}
	if code, body, _ := f.viewer.post(post, bad, "self"); code != 400 || !strings.Contains(body, "Yol tek bir") {
		t.Fatalf("bad path: %d", code)
	}
	empty := url.Values{"body": {" "}, "csrf": form["csrf"]}
	if code, body, _ := f.viewer.post(post, empty, "self"); code != 400 || !strings.Contains(body, "Yorum boş olamaz") {
		t.Fatalf("empty body: %d", code)
	}
	if code, _, _ := f.viewer.post(fmt.Sprintf("/deployments/%d/comments", queued.ID),
		url.Values{"body": {"x"}, "csrf": form["csrf"]}, "self"); code != http.StatusConflict {
		t.Fatalf("queued deployment: %d", code)
	}

	// The page lists the thread with its pin; mentions are highlighted,
	// and the comment is escaped.
	_, body, _ = f.viewer.get(review + "?thread=" + fmt.Sprint(th.ID))
	mustContain(t, "thread", body, `data-x="25.5"`, `data-y="40"`, `data-width="1280"`, `<span class="mention">@alice</span>`,
		"1 açık · 0 çözüldü", `class="thread st-open focus"`)

	// Replies through htmx return the sidebar fragment.
	csrf := form.Get("csrf")
	req, _ := http.NewRequest("POST", f.srv0()+fmt.Sprintf("/comments/%d/replies", th.ID),
		strings.NewReader(url.Values{"body": {"<b>tamam</b>"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := f.owner.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden { // owner's session has another CSRF token
		t.Fatalf("reply with a foreign CSRF token: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", f.srv0()+fmt.Sprintf("/comments/%d/replies", th.ID),
		strings.NewReader(url.Values{"body": {"<b>tamam</b>"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-CSRF-Token", f.owner.csrf(review))
	resp, err = f.owner.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b := new(strings.Builder)
	fmtCopy(b, resp)
	if resp.StatusCode != 200 || !strings.HasPrefix(strings.TrimSpace(b.String()), `<aside id="comments"`) ||
		!strings.Contains(b.String(), "&lt;b&gt;tamam&lt;/b&gt;") || strings.Contains(b.String(), "<b>tamam") {
		t.Fatalf("htmx reply: %d %s", resp.StatusCode, b)
	}

	// A viewer cannot resolve someone else's thread, but resolves her own;
	// the owner reopens it.
	ownerCSRF := f.owner.csrf(review)
	aliceID := f.ownerID(t)
	others, _ := f.st.CreateThread(ctx, d, store.NewComment{AuthorID: &aliceID, AuthorLogin: "alice", Body: "owner's"})
	if code, _, _ := f.viewer.post(fmt.Sprintf("/comments/%d/resolve", others.ID), url.Values{"csrf": {csrf}}, "self"); code != http.StatusForbidden {
		t.Fatalf("viewer resolves another's: %d", code)
	}
	if code, _, _ := f.viewer.post(fmt.Sprintf("/comments/%d/resolve", th.ID), url.Values{"csrf": {csrf}}, "self"); code != http.StatusSeeOther {
		t.Fatalf("viewer resolves own: %d", code)
	}
	if c, _ := f.st.GetComment(ctx, th.ID); c.Status != store.CommentResolved || c.ResolvedBy != "carol" {
		t.Fatalf("not resolved: %+v", c)
	}
	if code, _, _ := f.owner.post(fmt.Sprintf("/comments/%d/reopen", th.ID), url.Values{"csrf": {ownerCSRF}}, "self"); code != http.StatusSeeOther {
		t.Fatalf("owner reopens: %d", code)
	}
	// Delete: the viewer cannot delete the owner's thread; the owner can
	// (moderation); cross-origin is refused.
	if code, _, _ := f.viewer.post(fmt.Sprintf("/comments/%d/delete", others.ID), url.Values{"csrf": {csrf}}, "self"); code != http.StatusForbidden {
		t.Fatalf("viewer deletes another's: %d", code)
	}
	if code, _, _ := f.owner.post(fmt.Sprintf("/comments/%d/delete", others.ID), url.Values{"csrf": {ownerCSRF}}, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin delete: %d", code)
	}
	if code, _, _ := f.owner.post(fmt.Sprintf("/comments/%d/delete", others.ID), url.Values{"csrf": {ownerCSRF}}, "self"); code != http.StatusSeeOther {
		t.Fatalf("owner deletes: %d", code)
	}
	if _, err := f.st.GetComment(ctx, others.ID); err == nil {
		t.Fatal("thread not deleted")
	}

	// The deployments tab links the review page with the open count.
	_, body, _ = f.viewer.get("/apps/blog/deployments")
	mustContain(t, "deploys tab", body, `href="`+review+`"`, `title="1 açık yorum"`)
	_, body, _ = f.viewer.get(fmt.Sprintf("/deployments/%d", d.ID))
	mustContain(t, "deployment page", body, `href="`+review+`"`)
}
