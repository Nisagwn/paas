package web

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/terminal"
	"github.com/nisagwn/paas/internal/wsock"
)

// Faz 27: browser terminal.
//
//	GET  /apps/{name}/deployments/{id}/terminal   page (xterm.js), or, with
//	                                              Upgrade: websocket, the session
//	POST /apps/{name}/deployments/{id}/wake       wake a sleeping deployment
//
// Member+ only (viewers 403, other teams 404), browser sessions only (no
// bearer tokens), the WebSocket's Origin must be this host and the CSRF
// token arrives in ?csrf= or as the first message. Everything 404s when the
// terminal is off (PAAS_TERMINAL=false or the dry-run deployer).

// xterm.js, pinned with Subresource Integrity (unpkg is already allowed by
// the CSP for htmx).
const (
	xtermJS     = "https://unpkg.com/@xterm/xterm@5.5.0/lib/xterm.js"
	xtermJSSRI  = "sha384-M169f14mRZOXm3hD/v2Ti0ThIT/RnAQagXA9nlE15yHAtrW19gdePJh/HaTzUOe/"
	xtermCSS    = "https://unpkg.com/@xterm/xterm@5.5.0/css/xterm.css"
	xtermCSSSRI = "sha384-8Xk9wy/gzEDUKrXtrmCFa2bBuK3BpjpDuL/p0SeKQX19Khl/M+lHOgD/CyYf7efP"
	xtermFitJS  = "https://unpkg.com/@xterm/addon-fit@0.10.0/lib/addon-fit.js"
	xtermFitSRI = "sha384-iF+jqbuti4XlB64clWgFWYEscb+UnSRv3VgVikGYZu+otNFnSHr7y7NcKfBnGizn"
)

// terminalAuthTimeout bounds the wait for the browser's auth message.
const terminalAuthTimeout = 10 * time.Second

// terminalWakeTimeout bounds a wake from the terminal page.
const terminalWakeTimeout = 3 * time.Minute

// Deployment states for the terminal.
const (
	termReady    = "ready"
	termSleeping = "sleeping"
	termRetired  = "retired"
	termNotReady = "not_ready"
)

func terminalState(d store.Deployment) string {
	switch {
	case d.Status == store.StatusRetired || d.RetiredAt != nil:
		return termRetired
	case d.Status != store.StatusReady:
		return termNotReady
	case d.SleepingSince != nil:
		return termSleeping
	}
	return termReady
}

func (s *Server) terminalRoutes(mux *http.ServeMux) {
	s.pages["terminal"] = template.Must(template.New("").Funcs(funcs).
		ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/terminal.html"))
	mux.Handle("GET /apps/{name}/deployments/{id}/terminal", s.authed(s.terminal))
	mux.Handle("POST /apps/{name}/deployments/{id}/wake", s.authed(s.terminalWake))
}

// terminalTarget loads the app and deployment and checks the member role.
func (s *Server) terminalTarget(w http.ResponseWriter, r *http.Request) (store.App, store.Deployment, bool) {
	var d store.Deployment
	if s.Terminal == nil {
		s.errorPage(w, r, http.StatusNotFound, "Sayfa bulunamadı.")
		return store.App{}, d, false
	}
	app, err := s.Store.GetAppByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Proje bulunamadı.")
		return app, d, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return app, d, false
	}
	if !s.checkTeam(w, r, app.TeamID, store.RoleMember, "Proje bulunamadı.") {
		return app, d, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil && id > 0 {
		d, err = s.Store.GetDeployment(r.Context(), id)
	} else {
		err = store.ErrNotFound
	}
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.AppID != app.ID) {
		s.errorPage(w, r, http.StatusNotFound, "Deploy bulunamadı.")
		return app, d, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return app, d, false
	}
	return app, d, true
}

// terminalAllowed reports whether the deployment page shows the Terminal
// button: terminal on, deployment ready (awake or asleep), caller member+.
func (s *Server) terminalAllowed(r *http.Request, app store.App, d store.Deployment) bool {
	if s.Terminal == nil || d.Status != store.StatusReady || d.RetiredAt != nil {
		return false
	}
	role, err := s.Auth.TeamRole(r.Context(), me(r), app.TeamID)
	return err == nil && store.RoleAllows(role, store.RoleMember)
}

// terminalDetail fills the app page's terminal fields (members only).
func (s *Server) terminalDetail(ctx context.Context, v *appDetail) error {
	v.Terminal = s.Terminal != nil
	var err error
	v.TerminalSessions, err = s.Store.ListAudit(ctx, v.App.ID, store.AuditTerminal, 20)
	return err
}

func terminalPath(app string, id int64) string {
	return fmt.Sprintf("/apps/%s/deployments/%d/terminal", app, id)
}

func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	app, d, ok := s.terminalTarget(w, r)
	if !ok {
		return
	}
	if wsock.IsUpgrade(r) {
		s.terminalSocket(w, r, app, d)
		return
	}
	s.terminalPage(w, r, app, d, http.StatusOK, "")
}

type terminalPageData struct {
	App      store.App
	D        store.Deployment
	State    string
	Path     string
	CanWake  bool
	Pods     []terminal.Pod
	Procs    []string
	Selected terminal.Pod
	PodError string
	IdleMin  int
	MaxMin   int
	MaxUser  int
}

func (s *Server) terminalPage(w http.ResponseWriter, r *http.Request, app store.App, d store.Deployment, status int, errMsg string) {
	v := terminalPageData{
		App: app, D: d, State: terminalState(d), Path: terminalPath(app.Name, d.ID), CanWake: s.Terminal.Waker != nil,
		IdleMin: int(s.Terminal.IdleTimeout() / time.Minute), MaxMin: int(s.Terminal.MaxDuration() / time.Minute),
		MaxUser: s.Terminal.MaxPerUser(),
	}
	if v.State == termReady {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		pods, err := s.Terminal.Cluster.Pods(ctx, d)
		cancel()
		if err != nil {
			s.Log.Error("terminal: list pods", "app", app.Name, "deployment", d.ID, "err", err)
			v.PodError = "Pod'lar listelenemedi. Ayrıntılar sunucu logunda."
		} else {
			v.Pods, v.Procs = pods, terminal.Processes(pods)
			q := r.URL.Query()
			if v.Selected, err = terminal.Choose(pods, q.Get("process"), q.Get("pod")); err != nil {
				v.PodError = "Bu deploy'un çalışan (hazır) bir pod'u yok; pod yeniden başlıyor olabilir. Birkaç saniye sonra yenile."
			}
		}
	}
	w.Header().Set("Content-Security-Policy", terminalCSP(r))
	s.render(w, r, status, "terminal", app.Name+" · "+naming.ShortSHA(d.CommitSHA)+" · Terminal", v, errMsg)
}

// terminalCSP is the default policy plus xterm.css from unpkg and an
// explicit ws(s):// source for this host (older Safari does not count a
// WebSocket to the page's own host as 'self').
func terminalCSP(r *http.Request) string {
	nonce, _ := r.Context().Value(nonceKey).(string)
	connect := "'self'"
	if validHost(r.Host) {
		connect += " ws://" + r.Host + " wss://" + r.Host
	}
	return "default-src 'self'; script-src 'nonce-" + nonce + "' https://unpkg.com; " +
		"style-src 'self' 'unsafe-inline' https://unpkg.com; img-src 'self' data:; connect-src " + connect + "; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
}

func validHost(h string) bool {
	if h == "" || len(h) > 255 {
		return false
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(".-:[]", c)) {
			return false
		}
	}
	return true
}

// userKey names the caller for the per-user session limit.
func userKey(p auth.Principal) string {
	if p.Admin || p.UserID == 0 {
		return "admin"
	}
	return "u" + strconv.FormatInt(p.UserID, 10)
}

func (s *Server) terminalSocket(w http.ResponseWriter, r *http.Request, app store.App, d store.Deployment) {
	if r.Header.Get("Authorization") != "" {
		http.Error(w, "terminal yalnızca tarayıcı oturumuyla açılır", http.StatusForbidden)
		return
	}
	// Browsers always send Origin on a WebSocket; it must be this host
	// (cross-site WebSocket hijacking).
	if r.Header.Get("Origin") == "" || !auth.SameOrigin(r) {
		http.Error(w, "başka siteden gelen terminal bağlantısı reddedildi", http.StatusForbidden)
		return
	}
	if st := terminalState(d); st != termReady {
		http.Error(w, "terminal yalnızca çalışan (hazır, uyanık) deploy'larda açılır", http.StatusConflict)
		return
	}
	want := s.Sessions.CSRFToken(r)
	got := r.URL.Query().Get(auth.CSRFField)
	if got != "" && (want == "" || !hmac.Equal([]byte(got), []byte(want))) {
		http.Error(w, "geçersiz CSRF token'ı", http.StatusForbidden)
		return
	}
	conn, err := wsock.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade answered
	}
	if got == "" {
		tok, err := terminal.ReadAuth(conn, terminalAuthTimeout)
		if err != nil || want == "" || !hmac.Equal([]byte(tok), []byte(want)) {
			terminal.Refuse(conn, terminal.CodeForbidden, "csrf", "Oturum doğrulanamadı; sayfayı yenileyip tekrar dene.")
			return
		}
	}
	p := me(r)
	release, ok := s.Terminal.Acquire(userKey(p))
	if !ok {
		terminal.Refuse(conn, terminal.CodeTooMany, "too_many",
			fmt.Sprintf("Aynı anda en fazla %d terminal oturumu açabilirsin; önce birini kapat.", s.Terminal.MaxPerUser()))
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	pods, err := s.Terminal.Cluster.Pods(ctx, d)
	cancel()
	var pod terminal.Pod
	if err == nil {
		pod, err = terminal.Choose(pods, r.URL.Query().Get("process"), r.URL.Query().Get("pod"))
	}
	if err != nil {
		if !errors.Is(err, terminal.ErrNoPod) {
			s.Log.Error("terminal: list pods", "app", app.Name, "deployment", d.ID, "err", err)
		}
		terminal.Refuse(conn, terminal.CodeNoPod, "no_pod", "Bu deploy'un çalışan (hazır) bir pod'u yok; birkaç saniye sonra yeniden bağlan.")
		return
	}
	actor := p.Login
	if p.Admin || actor == "" {
		actor = "admin"
	}
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	size := terminal.Size{}
	if cols > 0 && rows > 0 && cols <= 1000 && rows <= 1000 {
		size = terminal.Size{Cols: uint16(cols), Rows: uint16(rows)}
	}
	s.Terminal.Run(r.Context(), conn, terminal.Session{
		App: app, Deployment: d, Namespace: naming.Namespace(app.Name), Pod: pod,
		UserID: p.UserID, Actor: actor, Size: size,
	})
}

func (s *Server) terminalWake(w http.ResponseWriter, r *http.Request) {
	app, d, ok := s.terminalTarget(w, r)
	if !ok {
		return
	}
	if terminalState(d) != termSleeping {
		http.Redirect(w, r, terminalPath(app.Name, d.ID), http.StatusSeeOther)
		return
	}
	if s.Terminal.Waker == nil {
		s.terminalPage(w, r, app, d, http.StatusConflict, "Uyandırma bu kurulumda kullanılamıyor; siteyi açınca deploy uyanır.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), terminalWakeTimeout)
	defer cancel()
	if err := s.Terminal.Waker.Wake(ctx, d); err != nil {
		s.Log.Error("terminal: wake", "app", app.Name, "deployment", d.ID, "err", err)
		s.terminalPage(w, r, app, d, http.StatusBadGateway, "Deploy uyandırılamadı; birkaç saniye sonra tekrar dene.")
		return
	}
	s.Log.Info("terminal: woke deployment", "app", app.Name, "deployment", d.ID, "actor", who(r))
	http.Redirect(w, r, terminalPath(app.Name, d.ID), http.StatusSeeOther)
}
