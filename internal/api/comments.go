package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 25: preview comments.
//
//	GET    /api/deployments/{id}/comments   threads with replies (viewer)
//	POST   /api/deployments/{id}/comments   new thread (viewer: reviewers comment)
//	POST   /api/comments/{id}/replies       reply (viewer)
//	POST   /api/comments/{id}/resolve       member, or the thread's author
//	POST   /api/comments/{id}/reopen        member, or the thread's author
//	DELETE /api/comments/{id}               the author; team owners moderate
//	GET    /api/apps/{name}/comments        threads of every deployment (viewer)
//
// Authorization goes through the deployment's app team, like
// lookupDeployment: comments of other teams answer 404.

// CommentSync is told about every change of a thread so it can mirror it to
// GitHub (github.CommentSync). It must not block: the request is not held
// up by GitHub.
type CommentSync interface {
	CommentChanged(deploymentID, threadID int64)
}

// MaxCommentPath bounds the page path of a thread.
const MaxCommentPath = 1024

// CheckCommentBody trims body and checks its length and characters.
func CheckCommentBody(body string) (string, error) {
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	switch {
	case body == "":
		return "", errors.New("comment body is required")
	case !utf8.ValidString(body):
		return "", errors.New("comment body must be valid UTF-8")
	case utf8.RuneCountInString(body) > store.MaxCommentBody:
		return "", errors.New("comment body is longer than " + strconv.Itoa(store.MaxCommentBody) + " characters")
	}
	for _, r := range body {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", errors.New("comment body contains control characters")
		}
	}
	return body, nil
}

// CheckCommentPin validates where a thread is pinned and returns the
// normalized path ("" becomes "/"). x and y are percentages of the iframe
// viewport, both or neither; viewport is its width in CSS pixels.
func CheckCommentPin(path string, x, y *float64, viewport *int) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/"
	}
	switch {
	case !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, `\`):
		return "", errors.New(`path must start with a single "/" (e.g. /pricing)`)
	case len(path) > MaxCommentPath:
		return "", errors.New("path is longer than 1024 characters")
	case strings.IndexFunc(path, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return "", errors.New("path must not contain spaces")
	case (x == nil) != (y == nil):
		return "", errors.New("x and y must be given together")
	case x != nil && !validPct(*x), y != nil && !validPct(*y):
		return "", errors.New("x and y must be percentages between 0 and 100")
	case viewport != nil && (*viewport < 1 || *viewport > 10000):
		return "", errors.New("viewport_width must be between 1 and 10000")
	}
	return path, nil
}

func validPct(f float64) bool { return !math.IsNaN(f) && f >= 0 && f <= 100 }

var mentionRe = regexp.MustCompile(`(?:^|[^\w@/.])@([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))\b`)

// MentionedLogins returns the @logins in body, in order, without duplicates
// (case-insensitive).
func MentionedLogins(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range mentionRe.FindAllStringSubmatch(body, -1) {
		k := strings.ToLower(m[1])
		if !seen[k] {
			seen[k] = true
			out = append(out, m[1])
		}
	}
	return out
}

// CommentMentions resolves the @logins of body to members of the team, with
// their canonical spelling. Other @words stay plain text.
func CommentMentions(ctx context.Context, st *store.Store, teamID int64, body string) ([]string, error) {
	logins := MentionedLogins(body)
	if len(logins) == 0 {
		return []string{}, nil
	}
	members, err := st.TeamMembers(ctx, teamID)
	if err != nil {
		return nil, err
	}
	byLower := make(map[string]string, len(members))
	for _, m := range members {
		byLower[strings.ToLower(m.Login)] = m.Login
	}
	out := []string{}
	for _, l := range logins {
		if canon, ok := byLower[strings.ToLower(l)]; ok {
			out = append(out, canon)
		}
	}
	return out, nil
}

// CommentAuthor is how p is stored as an author: the admin has no user.
func CommentAuthor(p auth.Principal) (*int64, string) {
	if p.UserID == 0 {
		return nil, p.Login
	}
	id := p.UserID
	return &id, p.Login
}

// IsCommentAuthor reports whether p wrote c.
func IsCommentAuthor(p auth.Principal, c store.Comment) bool {
	if p.UserID != 0 {
		return c.AuthorID != nil && *c.AuthorID == p.UserID
	}
	return p.Admin && c.AuthorID == nil && c.AuthorLogin == p.Login
}

// CanResolveComment: members resolve any thread, reviewers their own.
func CanResolveComment(role string, p auth.Principal, c store.Comment) bool {
	return store.RoleAllows(role, store.RoleMember) || (store.RoleAllows(role, store.RoleViewer) && IsCommentAuthor(p, c))
}

// CanDeleteComment: authors delete their own comments; owners moderate.
func CanDeleteComment(role string, p auth.Principal, c store.Comment) bool {
	return store.RoleAllows(role, store.RoleOwner) || (store.RoleAllows(role, store.RoleViewer) && IsCommentAuthor(p, c))
}

// ---- handlers ----

func (s *Server) commentRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/deployments/{id}/comments", s.listComments)
	api.HandleFunc("POST /api/deployments/{id}/comments", s.createComment)
	api.HandleFunc("POST /api/comments/{id}/replies", s.replyComment)
	api.HandleFunc("POST /api/comments/{id}/resolve", s.resolveComment(store.CommentResolved))
	api.HandleFunc("POST /api/comments/{id}/reopen", s.resolveComment(store.CommentOpen))
	api.HandleFunc("DELETE /api/comments/{id}", s.deleteComment)
	api.HandleFunc("GET /api/apps/{name}/comments", s.requireApp(store.RoleViewer, s.listAppComments))
}

type commentsView struct {
	Counts  store.CommentCount `json:"counts"`
	Threads []store.Comment    `json:"threads"`
}

func checkStatusFilter(w http.ResponseWriter, r *http.Request) (string, bool) {
	status := r.URL.Query().Get("status")
	if status != "" && status != store.CommentOpen && status != store.CommentResolved {
		writeError(w, http.StatusBadRequest, `status must be "open" or "resolved"`)
		return "", false
	}
	return status, true
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDeployment(w, r)
	if !ok {
		return
	}
	status, ok := checkStatusFilter(w, r)
	if !ok {
		return
	}
	threads, err := s.Store.ListThreads(r.Context(), d.ID, status)
	if err != nil {
		s.internalError(w, err)
		return
	}
	counts, err := s.Store.CountThreads(r.Context(), d.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, commentsView{Counts: counts, Threads: threads})
}

func (s *Server) listAppComments(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	status, ok := checkStatusFilter(w, r)
	if !ok {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	threads, err := s.Store.ListAppThreads(r.Context(), app.ID, status, limit)
	if err != nil {
		s.internalError(w, err)
		return
	}
	counts, err := s.Store.CountAppThreads(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, commentsView{Counts: counts, Threads: threads})
}

type createCommentRequest struct {
	Body          string   `json:"body"`
	Path          string   `json:"path"`
	X             *float64 `json:"x"`
	Y             *float64 `json:"y"`
	ViewportWidth *int     `json:"viewport_width"`
}

// NewThread validates a thread for deployment d of app and stores it.
// Errors other than store and database errors are validation messages.
func NewThread(ctx context.Context, st *store.Store, app store.App, d store.Deployment, p auth.Principal,
	body, path string, x, y *float64, viewport *int) (store.Comment, error) {
	if d.Status != store.StatusReady {
		return store.Comment{}, &InputError{"comments need a ready deployment (status " + d.Status + ")"}
	}
	body, err := CheckCommentBody(body)
	if err != nil {
		return store.Comment{}, &InputError{err.Error()}
	}
	path, err = CheckCommentPin(path, x, y, viewport)
	if err != nil {
		return store.Comment{}, &InputError{err.Error()}
	}
	mentions, err := CommentMentions(ctx, st, app.TeamID, body)
	if err != nil {
		return store.Comment{}, err
	}
	authorID, login := CommentAuthor(p)
	return st.CreateThread(ctx, d, store.NewComment{
		AuthorID: authorID, AuthorLogin: login, Body: body, Mentions: mentions,
		Path: path, X: x, Y: y, ViewportWidth: viewport,
	})
}

// NewReply validates a reply to thread (or to the thread of a reply) and
// stores it.
func NewReply(ctx context.Context, st *store.Store, app store.App, thread store.Comment, p auth.Principal, body string) (store.Comment, error) {
	body, err := CheckCommentBody(body)
	if err != nil {
		return store.Comment{}, &InputError{err.Error()}
	}
	mentions, err := CommentMentions(ctx, st, app.TeamID, body)
	if err != nil {
		return store.Comment{}, err
	}
	id := thread.ID
	if thread.ThreadID != nil {
		id = *thread.ThreadID
	}
	authorID, login := CommentAuthor(p)
	return st.AddReply(ctx, id, store.NewComment{AuthorID: authorID, AuthorLogin: login, Body: body, Mentions: mentions})
}

// InputError is a validation failure of a comment (400, or 409 for a
// deployment that is not ready).
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func (s *Server) commentInputError(w http.ResponseWriter, err error) {
	var in *InputError
	switch {
	case errors.As(err, &in) && strings.HasPrefix(in.Msg, "comments need a ready deployment"):
		writeError(w, http.StatusConflict, in.Msg)
	case errors.As(err, &in):
		writeError(w, http.StatusBadRequest, in.Msg)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "comment not found")
	default:
		s.internalError(w, err)
	}
}

func (s *Server) createComment(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDeploymentAs(w, r, store.RoleViewer)
	if !ok {
		return
	}
	var req createCommentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	app, err := s.Store.GetAppByName(r.Context(), d.AppName)
	if err != nil {
		s.internalError(w, err)
		return
	}
	c, err := NewThread(r.Context(), s.Store, app, d, principal(r), req.Body, req.Path, req.X, req.Y, req.ViewportWidth)
	if err != nil {
		s.commentInputError(w, err)
		return
	}
	s.Log.Info("comment created", "deployment", d.ID, "thread", c.ID, "by", principal(r).Login)
	s.commentChanged(c.DeploymentID, c.ID)
	writeJSON(w, http.StatusCreated, c)
}

// lookupComment loads the {id} comment if the caller may read its
// deployment's app, and returns the caller's role on the app's team.
func (s *Server) lookupComment(w http.ResponseWriter, r *http.Request) (store.Comment, store.App, string, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return store.Comment{}, store.App{}, "", false
	}
	c, err := s.Store.GetComment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "comment not found")
		return c, store.App{}, "", false
	}
	if err != nil {
		s.internalError(w, err)
		return c, store.App{}, "", false
	}
	app, err := s.Store.GetAppByID(r.Context(), c.AppID)
	if err != nil {
		s.internalError(w, err)
		return c, app, "", false
	}
	if !s.checkTeam(w, r, app.TeamID, store.RoleViewer, "comment not found") {
		return c, app, "", false
	}
	role, err := s.Auth.TeamRole(r.Context(), principal(r), app.TeamID)
	if err != nil {
		s.internalError(w, err)
		return c, app, "", false
	}
	return c, app, role, true
}

type replyRequest struct {
	Body string `json:"body"`
}

func (s *Server) replyComment(w http.ResponseWriter, r *http.Request) {
	c, app, _, ok := s.lookupComment(w, r)
	if !ok {
		return
	}
	var req replyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	reply, err := NewReply(r.Context(), s.Store, app, c, principal(r), req.Body)
	if err != nil {
		s.commentInputError(w, err)
		return
	}
	s.Log.Info("comment reply", "thread", *reply.ThreadID, "reply", reply.ID, "by", principal(r).Login)
	s.commentChanged(reply.DeploymentID, *reply.ThreadID)
	writeJSON(w, http.StatusCreated, reply)
}

func (s *Server) resolveComment(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, _, role, ok := s.lookupComment(w, r)
		if !ok {
			return
		}
		if !c.IsThread() {
			writeError(w, http.StatusBadRequest, "only threads can be resolved or reopened, not replies")
			return
		}
		if !CanResolveComment(role, principal(r), c) {
			writeError(w, http.StatusForbidden, "resolving others' threads needs the member role on the team")
			return
		}
		t, err := s.Store.SetThreadStatus(r.Context(), c.ID, status, principal(r).Login)
		if err != nil {
			s.commentInputError(w, err)
			return
		}
		s.Log.Info("comment status", "thread", c.ID, "status", status, "by", principal(r).Login)
		s.commentChanged(t.DeploymentID, t.ID)
		writeJSON(w, http.StatusOK, t)
	}
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) {
	c, _, role, ok := s.lookupComment(w, r)
	if !ok {
		return
	}
	if !CanDeleteComment(role, principal(r), c) {
		writeError(w, http.StatusForbidden, "only the author (or a team owner) can delete a comment")
		return
	}
	if err := s.Store.DeleteComment(r.Context(), c.ID); err != nil {
		s.commentInputError(w, err)
		return
	}
	s.Log.Info("comment deleted", "comment", c.ID, "by", principal(r).Login)
	thread := c.ID
	if c.ThreadID != nil {
		thread = *c.ThreadID
	}
	s.commentChanged(c.DeploymentID, thread)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) commentChanged(deploymentID, threadID int64) {
	if s.Comments != nil {
		s.Comments.CommentChanged(deploymentID, threadID)
	}
}
