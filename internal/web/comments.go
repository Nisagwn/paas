package web

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 25: preview comments. The review page shows a ready deployment in a
// sandboxed iframe with a comment sidebar. The deployment is another
// origin, so the page cannot read the iframe's DOM, scroll position or
// location: a pin is stored as x/y percentages of the iframe viewport, the
// page path the reviewer typed and the iframe width.
//
//	GET  /deployments/{id}/review     the page (viewer)
//	POST /deployments/{id}/comments   new thread (viewer)
//	POST /comments/{id}/replies       reply (viewer)
//	POST /comments/{id}/resolve       member, or the thread's author
//	POST /comments/{id}/reopen        member, or the thread's author
//	POST /comments/{id}/delete        the author; team owners moderate
//
// The forms post with htmx and get the sidebar fragment back; without
// JavaScript they redirect to the page.

func (s *Server) commentRoutes(mux *http.ServeMux) {
	mux.Handle("GET /deployments/{id}/review", s.authed(s.reviewPage))
	mux.Handle("POST /deployments/{id}/comments", s.authed(s.createThread))
	mux.Handle("POST /comments/{id}/replies", s.authed(s.replyThread))
	mux.Handle("POST /comments/{id}/resolve", s.authed(s.setThreadStatus(store.CommentResolved)))
	mux.Handle("POST /comments/{id}/reopen", s.authed(s.setThreadStatus(store.CommentOpen)))
	mux.Handle("POST /comments/{id}/delete", s.authed(s.deleteComment))
}

// commentView is a thread or reply as the sidebar shows it.
type commentView struct {
	store.Comment
	// N numbers the threads of the page; the pin shows the same number.
	N         int
	BodyHTML  template.HTML
	CanDelete bool
	// Threads only. PinX/PinY are the pin in percent ("" when not pinned);
	// ReplyDraft keeps a rejected reply.
	CanResolve bool
	Focus      bool
	PinX, PinY string
	ReplyDraft string
	Replies    []commentView
	CSRF       string
}

type reviewData struct {
	D        store.Deployment
	App      store.App
	URL      string // the deployment's own address
	FrameURL string // URL + Path
	Path     string
	// Embeddable is false when the deployment would share the control
	// plane's origin; the sandbox could then be lifted, so no iframe.
	Embeddable bool
	Ready      bool
	Open       []commentView
	Resolved   []commentView
	Counts     store.CommentCount
	Members    []string
	CanWrite   bool
	Focus      int64
	FormError  string
	Form       map[string]string
}

// loadReviewDeployment loads the {id} deployment, its app and the caller's
// role; it needs viewer (reviewers comment too).
func (s *Server) loadReviewDeployment(w http.ResponseWriter, r *http.Request) (store.Deployment, store.App, string, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.errorPage(w, r, http.StatusNotFound, "Deploy bulunamadı.")
		return store.Deployment{}, store.App{}, "", false
	}
	d, err := s.Store.GetDeployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Deploy bulunamadı.")
		return d, store.App{}, "", false
	}
	if err != nil {
		s.internalError(w, r, err)
		return d, store.App{}, "", false
	}
	app, role, ok := s.commentApp(w, r, d.AppID, "Deploy bulunamadı.")
	return d, app, role, ok
}

func (s *Server) commentApp(w http.ResponseWriter, r *http.Request, appID int64, notFound string) (store.App, string, bool) {
	app, err := s.Store.GetAppByID(r.Context(), appID)
	if err != nil {
		s.internalError(w, r, err)
		return app, "", false
	}
	if !s.checkTeam(w, r, app.TeamID, store.RoleViewer, notFound) {
		return app, "", false
	}
	role, err := s.Auth.TeamRole(r.Context(), me(r), app.TeamID)
	if err != nil {
		s.internalError(w, r, err)
		return app, "", false
	}
	return app, role, true
}

// reviewCSP extends the page's policy: frames may load the platform's app
// hosts (*.<domain>), on this page only.
func (s *Server) reviewCSP(w http.ResponseWriter) {
	scheme := s.Scheme
	if scheme == "" {
		scheme = "https"
	}
	h := w.Header()
	if csp := h.Get("Content-Security-Policy"); csp != "" {
		h.Set("Content-Security-Policy", csp+"; frame-src "+scheme+"://*."+s.Domain)
	}
}

func (s *Server) reviewPage(w http.ResponseWriter, r *http.Request) {
	d, app, role, ok := s.loadReviewDeployment(w, r)
	if !ok {
		return
	}
	path, err := api.CheckCommentPin(r.URL.Query().Get("path"), nil, nil, nil)
	if err != nil {
		path = "/"
	}
	focus, _ := strconv.ParseInt(r.URL.Query().Get("thread"), 10, 64)
	s.renderReview(w, r, http.StatusOK, d, app, role, path, focus, "", nil)
}

func (s *Server) reviewData(r *http.Request, d store.Deployment, app store.App, role, path string, focus int64) (reviewData, error) {
	ctx := r.Context()
	v := reviewData{D: d, App: app, URL: naming.URL(s.Scheme, d.Host(s.Domain)), Path: path, Focus: focus,
		Ready: d.Status == store.StatusReady, CanWrite: store.RoleAllows(role, store.RoleMember)}
	v.Embeddable = !strings.EqualFold(d.Host(s.Domain), r.Host)
	threads, err := s.Store.ListThreads(ctx, d.ID, "")
	if err != nil {
		return v, err
	}
	if v.Counts, err = s.Store.CountThreads(ctx, d.ID); err != nil {
		return v, err
	}
	members, err := s.Store.TeamMembers(ctx, app.TeamID)
	if err != nil {
		return v, err
	}
	for _, m := range members {
		v.Members = append(v.Members, m.Login)
	}
	p, csrf := me(r), s.Sessions.CSRFToken(r)
	for i, t := range threads {
		if t.ID == focus && r.URL.Query().Get("path") == "" {
			v.Path = t.Path // a link to a thread opens its page
		}
		tv := commentView{Comment: t, N: i + 1, BodyHTML: commentHTML(t.Body, t.Mentions), CSRF: csrf, Focus: t.ID == focus,
			CanResolve: api.CanResolveComment(role, p, t), CanDelete: api.CanDeleteComment(role, p, t)}
		if t.X != nil && t.Y != nil {
			tv.PinX, tv.PinY = strconv.FormatFloat(*t.X, 'f', -1, 64), strconv.FormatFloat(*t.Y, 'f', -1, 64)
		}
		for _, rep := range t.Replies {
			tv.Replies = append(tv.Replies, commentView{Comment: rep, BodyHTML: commentHTML(rep.Body, rep.Mentions),
				CanDelete: api.CanDeleteComment(role, p, rep)})
		}
		if t.Status == store.CommentResolved {
			v.Resolved = append(v.Resolved, tv)
		} else {
			v.Open = append(v.Open, tv)
		}
	}
	v.FrameURL = v.URL + v.Path
	return v, nil
}

func (s *Server) renderReview(w http.ResponseWriter, r *http.Request, status int, d store.Deployment, app store.App,
	role, path string, focus int64, errMsg string, form map[string]string) {
	v, err := s.reviewData(r, d, app, role, path, focus)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	v.Form = form
	if to := form["ReplyTo"]; to != "" {
		for _, list := range [][]commentView{v.Open, v.Resolved} {
			for i := range list {
				if strconv.FormatInt(list[i].ID, 10) == to {
					list[i].ReplyDraft = form["Reply"]
				}
			}
		}
	}
	if isHTMX(r) {
		v.FormError = tr(errMsg)
		s.commentsFragment(w, r, status, v)
		return
	}
	s.reviewCSP(w)
	s.render(w, r, status, "review", fmt.Sprintf("%s · %s · İnceleme", d.AppName, naming.ShortSHA(d.CommitSHA)), v, errMsg)
}

// commentsFragment renders the sidebar for htmx.
func (s *Server) commentsFragment(w http.ResponseWriter, r *http.Request, status int, v reviewData) {
	var buf bytes.Buffer
	if err := s.pages["review"].ExecuteTemplate(&buf, "comments", page{Data: v, CSRF: s.Sessions.CSRFToken(r)}); err != nil {
		s.Log.Error("render", "partial", "comments", "err", err)
		http.Error(w, "sunucu hatası", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// afterComment answers a successful comment form: the sidebar for htmx,
// otherwise a redirect to the review page at the thread.
func (s *Server) afterComment(w http.ResponseWriter, r *http.Request, d store.Deployment, app store.App, role, path string, thread int64) {
	if isHTMX(r) {
		s.renderReview(w, r, http.StatusOK, d, app, role, path, thread, "", nil)
		return
	}
	q := url.Values{"path": {path}}
	if thread != 0 {
		q.Set("thread", strconv.FormatInt(thread, 10))
	}
	target := fmt.Sprintf("/deployments/%d/review?%s", d.ID, q.Encode())
	if thread != 0 {
		target += fmt.Sprintf("#c-%d", thread)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) commentChanged(deploymentID, threadID int64) {
	if s.Comments != nil {
		s.Comments.CommentChanged(deploymentID, threadID)
	}
}

func formFloat(r *http.Request, name string) (*float64, bool) {
	v := strings.TrimSpace(r.PostFormValue(name))
	if v == "" {
		return nil, true
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil, false
	}
	return &f, true
}

func (s *Server) createThread(w http.ResponseWriter, r *http.Request) {
	d, app, role, ok := s.loadReviewDeployment(w, r)
	if !ok {
		return
	}
	body, path := r.PostFormValue("body"), r.PostFormValue("path")
	form := map[string]string{"Body": body}
	safePath, perr := api.CheckCommentPin(path, nil, nil, nil)
	if perr != nil {
		safePath = "/"
	}
	fail := func(status int, msg string) {
		s.renderReview(w, r, status, d, app, role, safePath, 0, msg, form)
	}
	x, okX := formFloat(r, "x")
	y, okY := formFloat(r, "y")
	var viewport *int
	if v := strings.TrimSpace(r.PostFormValue("viewport_width")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			fail(http.StatusBadRequest, "viewport_width must be between 1 and 10000")
			return
		}
		viewport = &n
	}
	if !okX || !okY {
		fail(http.StatusBadRequest, "x and y must be percentages between 0 and 100")
		return
	}
	c, err := api.NewThread(r.Context(), s.Store, app, d, me(r), body, path, x, y, viewport)
	var in *api.InputError
	if errors.As(err, &in) {
		status := http.StatusBadRequest
		if d.Status != store.StatusReady {
			status = http.StatusConflict
		}
		fail(status, in.Msg)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("comment created", "deployment", d.ID, "thread", c.ID, "by", me(r).Login, "via", "web")
	s.commentChanged(d.ID, c.ID)
	s.afterComment(w, r, d, app, role, c.Path, c.ID)
}

// loadComment loads the {id} comment, its deployment and app, checking
// that the caller may read the app.
func (s *Server) loadComment(w http.ResponseWriter, r *http.Request) (store.Comment, store.Deployment, store.App, string, bool) {
	var d store.Deployment
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.errorPage(w, r, http.StatusNotFound, "Yorum bulunamadı.")
		return store.Comment{}, d, store.App{}, "", false
	}
	c, err := s.Store.GetComment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Yorum bulunamadı.")
		return c, d, store.App{}, "", false
	}
	if err != nil {
		s.internalError(w, r, err)
		return c, d, store.App{}, "", false
	}
	app, role, ok := s.commentApp(w, r, c.AppID, "Yorum bulunamadı.")
	if !ok {
		return c, d, app, role, false
	}
	if d, err = s.Store.GetDeployment(r.Context(), c.DeploymentID); err != nil {
		s.internalError(w, r, err)
		return c, d, app, role, false
	}
	return c, d, app, role, true
}

// threadOf returns the thread c belongs to (c itself for a thread).
func (s *Server) threadOf(r *http.Request, c store.Comment) (store.Comment, error) {
	if c.IsThread() {
		return c, nil
	}
	return s.Store.GetComment(r.Context(), *c.ThreadID)
}

func (s *Server) replyThread(w http.ResponseWriter, r *http.Request) {
	c, d, app, role, ok := s.loadComment(w, r)
	if !ok {
		return
	}
	t, err := s.threadOf(r, c)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	body := r.PostFormValue("body")
	reply, err := api.NewReply(r.Context(), s.Store, app, t, me(r), body)
	var in *api.InputError
	if errors.As(err, &in) {
		s.renderReview(w, r, http.StatusBadRequest, d, app, role, t.Path, t.ID, in.Msg,
			map[string]string{"Reply": body, "ReplyTo": strconv.FormatInt(t.ID, 10)})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("comment reply", "thread", t.ID, "reply", reply.ID, "by", me(r).Login, "via", "web")
	s.commentChanged(d.ID, t.ID)
	s.afterComment(w, r, d, app, role, t.Path, t.ID)
}

func (s *Server) setThreadStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, d, app, role, ok := s.loadComment(w, r)
		if !ok {
			return
		}
		if !c.IsThread() {
			s.renderReview(w, r, http.StatusBadRequest, d, app, role, "/", 0, "Yalnızca yorum dizileri çözülebilir.", nil)
			return
		}
		if !api.CanResolveComment(role, me(r), c) {
			s.renderReview(w, r, http.StatusForbidden, d, app, role, c.Path, c.ID,
				"Başkalarının yorumlarını çözmek için ekipte üye rolü gerekir.", nil)
			return
		}
		if _, err := s.Store.SetThreadStatus(r.Context(), c.ID, status, me(r).Login); err != nil {
			s.internalError(w, r, err)
			return
		}
		s.Log.Info("comment status", "thread", c.ID, "status", status, "by", me(r).Login, "via", "web")
		s.commentChanged(d.ID, c.ID)
		s.afterComment(w, r, d, app, role, c.Path, c.ID)
	}
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) {
	c, d, app, role, ok := s.loadComment(w, r)
	if !ok {
		return
	}
	t, err := s.threadOf(r, c)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !api.CanDeleteComment(role, me(r), c) {
		s.renderReview(w, r, http.StatusForbidden, d, app, role, t.Path, t.ID,
			"Yalnızca yorumun yazarı (ya da ekip sahibi) silebilir.", nil)
		return
	}
	if err := s.Store.DeleteComment(r.Context(), c.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("comment deleted", "comment", c.ID, "by", me(r).Login, "via", "web")
	s.commentChanged(d.ID, t.ID)
	focus := t.ID
	if c.IsThread() {
		focus = 0
	}
	s.afterComment(w, r, d, app, role, t.Path, focus)
}

var mentionHTMLRe = regexp.MustCompile(`@([A-Za-z0-9][A-Za-z0-9-]*)`)

// commentHTML escapes a comment and highlights the mentions of team
// members; line breaks are kept by CSS (white-space: pre-wrap).
func commentHTML(body string, mentions []string) template.HTML {
	ok := map[string]bool{}
	for _, m := range mentions {
		ok[strings.ToLower(m)] = true
	}
	esc := template.HTMLEscapeString(body)
	return template.HTML(mentionHTMLRe.ReplaceAllStringFunc(esc, func(m string) string {
		if ok[strings.ToLower(m[1:])] {
			return `<span class="mention">` + m + `</span>`
		}
		return m
	}))
}
