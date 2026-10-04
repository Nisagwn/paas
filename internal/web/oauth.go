package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/auth"
)

// Faz 13: "Sign in with GitHub" (OAuth authorization code flow + PKCE).
//
// /auth/github stores a random state, the PKCE verifier and the return path
// in a signed cookie valid for 10 minutes and sends the browser to GitHub.
// /auth/github/callback accepts the code only if the state matches that
// cookie, so a login cannot be forced onto someone else's browser.

const (
	oauthCookie = "paas_oauth"
	oauthTTL    = 10 * time.Minute
)

// loginErrors are fixed messages selected by /login?error=.
var loginErrors = map[string]string{
	"denied":      "GitHub sign-in was cancelled.",
	"state":       "The sign-in attempt expired or did not start here. Please try again.",
	"not_allowed": "This GitHub account is not allowed to sign in. Ask an owner to add you to a team.",
	"failed":      "Signing in with GitHub failed. The server log has details.",
}

func randomString() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) githubStart(w http.ResponseWriter, r *http.Request) {
	if s.GitHub == nil {
		s.errorPage(w, r, http.StatusNotFound, "GitHub sign-in is not configured.")
		return
	}
	state, verifier := randomString(), randomString()
	next := safeNext(r.URL.Query().Get("next"))
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookie, Value: s.Sessions.Seal("oauth", state+" "+verifier+" "+next, oauthTTL),
		Path: "/auth/github", MaxAge: int(oauthTTL.Seconds()), HttpOnly: true,
		// Lax: the callback is a top-level navigation coming from github.com.
		SameSite: http.SameSiteLaxMode, Secure: auth.IsTLS(r),
	})
	http.Redirect(w, r, s.GitHub.App.AuthorizeURL(s.GitHub.RedirectURI, state, auth.PKCEChallenge(verifier), s.GitHub.Scope()),
		http.StatusFound)
}

func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request) {
	if s.GitHub == nil {
		s.errorPage(w, r, http.StatusNotFound, "GitHub sign-in is not configured.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: "", Path: "/auth/github", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: auth.IsTLS(r)})

	var saved string
	if c, err := r.Cookie(oauthCookie); err == nil {
		saved, _ = s.Sessions.Open("oauth", c.Value)
	}
	parts := strings.SplitN(saved, " ", 3)
	q := r.URL.Query()
	if len(parts) != 3 || subtle.ConstantTimeCompare([]byte(parts[0]), []byte(q.Get("state"))) != 1 {
		s.Log.Warn("web: oauth state mismatch", "remote", r.RemoteAddr)
		s.loginFailed(w, r, http.StatusBadRequest, "state")
		return
	}
	verifier, next := parts[1], safeNext(parts[2])
	if q.Get("error") != "" || q.Get("code") == "" {
		s.loginFailed(w, r, http.StatusUnauthorized, "denied")
		return
	}
	u, err := s.GitHub.Complete(r.Context(), s.Store, q.Get("code"), verifier)
	if errors.Is(err, auth.ErrNotAllowed) {
		s.Log.Warn("web: github login refused", "remote", r.RemoteAddr)
		s.loginFailed(w, r, http.StatusForbidden, "not_allowed")
		return
	}
	if err != nil {
		s.Log.Error("web: github login", "err", err)
		s.loginFailed(w, r, http.StatusBadGateway, "failed")
		return
	}
	s.Log.Info("web: github login", "user", u.Login, "id", u.ID)
	s.Sessions.IssueUser(w, r, u.ID)
	// A plain redirect would still count as part of the cross-site
	// navigation from github.com, and the SameSite=Strict session cookie
	// would not be sent with it. A same-site page that navigates on is.
	s.render(w, r, http.StatusOK, "redirect", "Signing in", map[string]string{"Next": next}, "")
}

func (s *Server) loginFailed(w http.ResponseWriter, r *http.Request, status int, code string) {
	s.render(w, r, status, "login", "Log in", s.loginData("/"), loginErrors[code])
}
