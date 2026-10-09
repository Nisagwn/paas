package protect

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
)

const (
	// CookieName is the access cookie on an app host.
	CookieName = "_paas_protect"
	// BypassHeader carries the app's bypass secret (automation).
	BypassHeader = "X-Paas-Protection-Bypass"
	// ShareParam is the query parameter of a shareable link.
	ShareParam = "paas_share"
	// AuthPath on an app host redeems a one-time login token. Protected
	// apps cannot serve this path themselves.
	AuthPath = "/_paas/auth"
	// VerifyPath is the ForwardAuth endpoint on the control plane.
	VerifyPath = "/_auth/verify"
	// LoginPath is the login page on the control plane (internal/web).
	LoginPath = "/_protect/login"

	// Access kinds bound into cookies and tokens.
	KindTeam     = "team"
	KindPassword = "password"
	KindShare    = "share"

	// DefaultCookieTTL is how long access lasts (like a platform session).
	DefaultCookieTTL = 12 * time.Hour
	// TokenTTL bounds the redirect from the login page to the app host.
	TokenTTL = 2 * time.Minute
	// DefaultCacheTTL: settings, memberships and share links are read at
	// most this often per app, so a change takes effect within it.
	DefaultCacheTTL = 5 * time.Second
)

// Guard checks requests to protected hosts (ServeVerify) and issues the
// tokens of the login page.
type Guard struct {
	Store *store.Store
	// Sessions signs cookies and tokens (key derived from PAAS_SESSION_KEY).
	Sessions *auth.Sessions
	// Auth tells whether an admin session (user 0, dev-mode token login)
	// counts as a member; nil: it does not.
	Auth *auth.Authenticator
	// Domain is the platform domain (deployment hosts are checked against it).
	Domain string
	// PublicURL is the control plane's address in browsers; the login page
	// is PublicURL + LoginPath.
	PublicURL string
	Log       *slog.Logger
	// CookieTTL: zero means DefaultCookieTTL. CacheTTL: zero means
	// DefaultCacheTTL, negative disables caching (tests).
	CookieTTL time.Duration
	CacheTTL  time.Duration
	// MaxFailures password attempts per client and app within
	// FailureWindow; zero means 10 per 15 minutes.
	MaxFailures   int
	FailureWindow time.Duration

	mu       sync.Mutex
	cache    map[string]cached
	failures map[string]*failure
}

type cached struct {
	val any
	exp time.Time
}

type failure struct {
	n     int
	reset time.Time
}

func (g *Guard) cookieTTL() time.Duration {
	if g.CookieTTL > 0 {
		return g.CookieTTL
	}
	return DefaultCookieTTL
}

// remember returns the cached value of key or loads and caches it.
func (g *Guard) remember(key string, load func() (any, error)) (any, error) {
	ttl := g.CacheTTL
	if ttl == 0 {
		ttl = DefaultCacheTTL
	}
	now := time.Now()
	if ttl > 0 {
		g.mu.Lock()
		c, ok := g.cache[key]
		g.mu.Unlock()
		if ok && now.Before(c.exp) {
			return c.val, nil
		}
	}
	v, err := load()
	if err != nil || ttl < 0 {
		return v, err
	}
	g.mu.Lock()
	if g.cache == nil || len(g.cache) > 10_000 {
		g.cache = map[string]cached{}
	}
	g.cache[key] = cached{val: v, exp: now.Add(ttl)}
	g.mu.Unlock()
	return v, nil
}

// Forget drops the cached setting of an app, so a change made in this
// process applies at once.
func (g *Guard) Forget(app string) {
	g.mu.Lock()
	delete(g.cache, "p:"+app)
	g.mu.Unlock()
}

func (g *Guard) protection(ctx context.Context, app string) (store.Protection, error) {
	v, err := g.remember("p:"+app, func() (any, error) { return g.Store.ProtectionByAppName(ctx, app) })
	if err != nil {
		return store.Protection{}, err
	}
	return v.(store.Protection), nil
}

func (g *Guard) isMember(ctx context.Context, teamID, userID int64) (bool, error) {
	if userID == 0 {
		return g.Auth != nil && g.Auth.TokenLogin(), nil
	}
	v, err := g.remember(fmt.Sprintf("m:%d:%d", teamID, userID), func() (any, error) {
		role, err := g.Store.TeamRole(ctx, userID, teamID)
		return role != "", err
	})
	if err != nil {
		return false, err
	}
	return v.(bool), nil
}

func (g *Guard) shareValid(ctx context.Context, appID, id int64) (bool, error) {
	v, err := g.remember(fmt.Sprintf("s:%d:%d", appID, id), func() (any, error) {
		l, err := g.Store.GetShareLink(ctx, appID, id)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return err == nil && !l.Expired(time.Now()), err
	})
	if err != nil {
		return false, err
	}
	return v.(bool), nil
}

// ---- cookies and tokens ----

// grant is what an access cookie or a login token says.
type grant struct {
	Host    string
	Version int64
	Kind    string
	Subject string // user id (team), "" (password), share link id (share)
}

func (gr grant) encode(extra ...string) string {
	return strings.Join(append([]string{gr.Host, strconv.FormatInt(gr.Version, 10), gr.Kind, gr.Subject}, extra...), "\n")
}

func decodeGrant(v string, extra int) (grant, []string, bool) {
	parts := strings.Split(v, "\n")
	if len(parts) != 4+extra {
		return grant{}, nil, false
	}
	ver, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return grant{}, nil, false
	}
	return grant{Host: parts[0], Version: ver, Kind: parts[2], Subject: parts[3]}, parts[4:], true
}

func cookiePurpose(app string) string { return "protect-cookie " + app }
func tokenPurpose(app string) string  { return "protect-token " + app }

// accessCookie is the signed cookie for gr on its host.
func (g *Guard) accessCookie(app string, gr grant, secure bool) *http.Cookie {
	ttl := g.cookieTTL()
	return &http.Cookie{
		Name: CookieName, Value: g.Sessions.Seal(cookiePurpose(app), gr.encode(), ttl),
		Path: "/", MaxAge: int(ttl.Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	}
}

// sealCookieValue is accessCookie's value with an explicit lifetime (tests).
func (g *Guard) sealCookieValue(app string, gr grant, ttl time.Duration) string {
	return g.Sessions.Seal(cookiePurpose(app), gr.encode(), ttl)
}

// allows reports whether a grant still admits a request to host under p.
func (g *Guard) allows(ctx context.Context, p store.Protection, gr grant, host string) (bool, error) {
	if !strings.EqualFold(gr.Host, host) || gr.Version != p.Version {
		return false, nil
	}
	switch gr.Kind {
	case KindTeam: // members pass in both modes
		uid, err := strconv.ParseInt(gr.Subject, 10, 64)
		if err != nil || uid < 0 {
			return false, nil
		}
		return g.isMember(ctx, p.TeamID, uid)
	case KindPassword:
		return p.Mode == store.ProtectPassword, nil
	case KindShare:
		id, err := strconv.ParseInt(gr.Subject, 10, 64)
		if err != nil {
			return false, nil
		}
		return g.shareValid(ctx, p.AppID, id)
	}
	return false, nil
}

// LoginTarget validates a login request: the app must exist and next must
// be an address of it. The protection setting comes with it.
func (g *Guard) LoginTarget(ctx context.Context, appName, next string) (store.App, store.Protection, *url.URL, error) {
	app, err := g.Store.GetAppByName(ctx, appName)
	if err != nil {
		return app, store.Protection{}, nil, err
	}
	p, err := g.Store.GetProtection(ctx, app.ID)
	if err != nil {
		return app, p, nil, err
	}
	u, err := AppURL(ctx, g.Store, app, g.Domain, next)
	return app, p, u, err
}

// TokenURL is where the login page sends the browser after a successful
// login: the target's host with a one-time token that sets the cookie.
// kind is KindTeam (subject: user id) or KindPassword.
func (g *Guard) TokenURL(app store.App, p store.Protection, target *url.URL, kind, subject string) string {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	next := target.EscapedPath()
	if next == "" {
		next = "/"
	}
	if target.RawQuery != "" {
		next += "?" + target.RawQuery
	}
	gr := grant{Host: strings.ToLower(target.Host), Version: p.Version, Kind: kind, Subject: subject}
	tok := g.Sessions.Seal(tokenPurpose(app.Name), gr.encode(b64.EncodeToString(nonce), next), TokenTTL)
	return (&url.URL{Scheme: target.Scheme, Host: target.Host, Path: AuthPath,
		RawQuery: url.Values{"token": {tok}}.Encode()}).String()
}

// LoginURL is the login page for a request to rawURL of app.
func (g *Guard) LoginURL(app, rawURL string) string {
	return strings.TrimRight(g.PublicURL, "/") + LoginPath + "?" + url.Values{"app": {app}, "next": {rawURL}}.Encode()
}

// ---- password attempts ----

// ClientKey identifies a client for the password limit: the first
// X-Forwarded-For entry (set by Traefik), else the peer address.
func ClientKey(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		first, _, _ := strings.Cut(f, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ErrTooManyAttempts: the client typed a wrong password too often.
var ErrTooManyAttempts = errors.New("too many wrong passwords; try again later")

// CheckPassword checks a password typed for app p by client. Wrong
// passwords count against the client; at the limit every attempt is
// refused with ErrTooManyAttempts until the window ends.
func (g *Guard) CheckPassword(p store.Protection, client, password string) (bool, error) {
	max, window := g.MaxFailures, g.FailureWindow
	if max <= 0 {
		max = 10
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	key := strconv.FormatInt(p.AppID, 10) + " " + client
	now := time.Now()
	g.mu.Lock()
	f := g.failures[key]
	if f != nil && now.After(f.reset) {
		delete(g.failures, key)
		f = nil
	}
	limited := f != nil && f.n >= max
	g.mu.Unlock()
	if limited {
		return false, ErrTooManyAttempts
	}
	if p.Mode == store.ProtectPassword && CheckPassword(p.PasswordHash, password) {
		return true, nil
	}
	g.mu.Lock()
	if g.failures == nil || len(g.failures) > 10_000 {
		g.failures = map[string]*failure{}
	}
	if f = g.failures[key]; f == nil {
		f = &failure{reset: now.Add(window)}
		g.failures[key] = f
	}
	f.n++
	g.mu.Unlock()
	return false, nil
}

// ---- the ForwardAuth endpoint ----

// ServeHTTP is ServeVerify, so a Guard mounts as the /_auth/verify handler.
func (g *Guard) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.ServeVerify(w, r) }

// ServeVerify answers Traefik's ForwardAuth subrequest. The app comes from
// the Middleware's address (?app=<name>); the original request from the
// X-Forwarded-* headers. 2xx lets the request through; any other answer,
// headers and body included, goes to the browser as is.
func (g *Guard) ServeVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	appName := r.URL.Query().Get("app")
	p, err := g.protection(ctx, appName)
	if errors.Is(err, store.ErrNotFound) {
		deny(w, http.StatusNotFound, "Proje bulunamadı.", "")
		return
	}
	if err != nil {
		g.Log.Error("protect: verify", "app", appName, "err", err)
		deny(w, http.StatusServiceUnavailable, "Erişim şu an doğrulanamıyor; biraz sonra tekrar dene.", "")
		return
	}
	if !p.Enabled() {
		w.WriteHeader(http.StatusOK)
		return
	}
	host := strings.ToLower(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		deny(w, http.StatusForbidden, "Bu adres yalnızca yönlendirici üzerinden çağrılır.", "")
		return
	}
	scheme := "http"
	if auth.IsTLS(r) {
		scheme = "https"
	}
	uri := r.Header.Get("X-Forwarded-Uri")
	if uri == "" {
		uri = "/"
	}
	u, err := url.ParseRequestURI(uri)
	if err != nil {
		u = &url.URL{Path: "/"}
	}

	if secretMatches(r.Header.Get(BypassHeader), p.BypassHash) {
		w.WriteHeader(http.StatusOK)
		return
	}
	if u.Path == AuthPath {
		g.redeem(w, r, p, host, scheme, u)
		return
	}
	if tok := u.Query().Get(ShareParam); tok != "" {
		g.share(w, r, p, host, scheme, u, tok)
		return
	}
	if c, err := r.Cookie(CookieName); err == nil {
		if v, ok := g.Sessions.Open(cookiePurpose(p.AppName), c.Value); ok {
			if gr, _, ok := decodeGrant(v, 0); ok {
				allowed, err := g.allows(ctx, p, gr, host)
				if err != nil {
					g.Log.Error("protect: verify", "app", p.AppName, "err", err)
					deny(w, http.StatusServiceUnavailable, "Erişim şu an doğrulanamıyor; biraz sonra tekrar dene.", "")
					return
				}
				if allowed {
					w.WriteHeader(http.StatusOK)
					return
				}
			}
		}
	}

	method := r.Header.Get("X-Forwarded-Method")
	if method == "" || method == http.MethodGet || method == http.MethodHead {
		http.Redirect(w, r, g.LoginURL(p.AppName, scheme+"://"+host+u.RequestURI()), http.StatusFound)
		return
	}
	deny(w, http.StatusUnauthorized, "Bu deploy korumalı. Önce tarayıcıda açıp giriş yap.", "")
}

// redeem turns a one-time login token into the host's cookie.
func (g *Guard) redeem(w http.ResponseWriter, r *http.Request, p store.Protection, host, scheme string, u *url.URL) {
	retry := g.LoginURL(p.AppName, scheme+"://"+host+"/")
	v, ok := g.Sessions.Open(tokenPurpose(p.AppName), u.Query().Get("token"))
	gr, extra, ok2 := decodeGrant(v, 2)
	if !ok || !ok2 || !strings.EqualFold(gr.Host, host) || gr.Version != p.Version ||
		(gr.Kind != KindTeam && gr.Kind != KindPassword) {
		deny(w, http.StatusForbidden, "Giriş bağlantısı geçersiz ya da süresi dolmuş.", retry)
		return
	}
	nonce, next := extra[0], extra[1]
	fresh, err := g.Store.UseNonce(r.Context(), nonce, time.Now().Add(TokenTTL))
	if err != nil {
		g.Log.Error("protect: redeem", "app", p.AppName, "err", err)
		deny(w, http.StatusServiceUnavailable, "Erişim şu an doğrulanamıyor; biraz sonra tekrar dene.", "")
		return
	}
	if !fresh {
		deny(w, http.StatusForbidden, "Bu giriş bağlantısı zaten kullanıldı.", retry)
		return
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/"
	}
	http.SetCookie(w, g.accessCookie(p.AppName, gr, scheme == "https"))
	g.Log.Info("protect: access granted", "app", p.AppName, "host", host, "kind", gr.Kind)
	// Absolute: Traefik resolves a relative Location against the
	// ForwardAuth address, not the app host.
	http.Redirect(w, r, scheme+"://"+host+next, http.StatusFound)
}

// share redeems a shareable link: the cookie, then the same URL without
// the token.
func (g *Guard) share(w http.ResponseWriter, r *http.Request, p store.Protection, host, scheme string, u *url.URL, tok string) {
	l, err := g.Store.UseShareLink(r.Context(), p.AppID, HashSecret(tok))
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrExpired):
		deny(w, http.StatusForbidden, "Bu paylaşım bağlantısı geçersiz, iptal edilmiş ya da süresi dolmuş.",
			g.LoginURL(p.AppName, scheme+"://"+host+"/"))
		return
	case err != nil:
		g.Log.Error("protect: share", "app", p.AppName, "err", err)
		deny(w, http.StatusServiceUnavailable, "Erişim şu an doğrulanamıyor; biraz sonra tekrar dene.", "")
		return
	}
	gr := grant{Host: host, Version: p.Version, Kind: KindShare, Subject: strconv.FormatInt(l.ID, 10)}
	http.SetCookie(w, g.accessCookie(p.AppName, gr, scheme == "https"))
	q := u.Query()
	q.Del(ShareParam)
	clean := &url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: q.Encode()}
	g.Log.Info("protect: access granted", "app", p.AppName, "host", host, "kind", KindShare, "link", l.ID)
	http.Redirect(w, r, scheme+"://"+host+clean.RequestURI(), http.StatusFound)
}

// deny writes a Turkish page; Traefik shows it on the app host.
func deny(w http.ResponseWriter, status int, msg, retry string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	link := ""
	if retry != "" {
		link = `<p><a href="` + html.EscapeString(retry) + `">Tekrar giriş yap</a></p>`
	}
	fmt.Fprintf(w, `<!doctype html><html lang="tr"><head><meta charset="utf-8">`+
		`<meta name="viewport" content="width=device-width,initial-scale=1"><title>Korumalı deploy</title>`+
		`<style>body{font-family:system-ui,sans-serif;max-width:480px;margin:15vh auto;padding:0 16px;color:#222;background:#fff}`+
		`@media (prefers-color-scheme:dark){body{color:#eee;background:#111}a{color:#8ab4ff}}</style></head>`+
		`<body><h1>Korumalı deploy</h1><p>%s</p>%s</body></html>`, html.EscapeString(msg), link)
}
