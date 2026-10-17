package web

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/nisagwn/paas/internal/auth"
)

// Sign-in page (/login). GitHub is the way in for everyone; the API token
// form exists only while token login is enabled (no GitHub OAuth) and is
// then the platform operator's door, so it is folded away under GitHub.

// authTemplates parses the shared parts of /login and /signup
// (templates/auth_parts.html) and the signup page.
func (s *Server) authTemplates() {
	template.Must(s.pages["login"].ParseFS(templateFS, "templates/auth_parts.html"))
	s.pages["signup"] = template.Must(template.New("").Funcs(funcs).
		ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/processes.html",
			"templates/rollout.html", "templates/auth_parts.html", "templates/signup.html"))
}

// signedIn reports whether the request carries a valid session.
func (s *Server) signedIn(r *http.Request) bool {
	_, err := s.Auth.Session(r)
	return err == nil
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if s.signedIn(r) {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	code := r.URL.Query().Get("error")
	s.render(w, r, http.StatusOK, "login", "Giriş yap", s.loginView(next, code), loginErrors[code])
}

// loginData selects the login method: GitHub, or the token form in dev mode.
func (s *Server) loginData(next string) map[string]any { return s.loginView(next, "") }

// loginView is loginData with the help line of a known error code.
func (s *Server) loginView(next, code string) map[string]any {
	github, token := s.GitHub != nil, s.Auth.TokenLogin()
	d := map[string]any{
		"Next": next, "GitHub": github, "TokenLogin": token,
		// The token form is open when it is the only way in or its
		// last attempt failed.
		"TokenOpen": token && (!github || code == "bad_token"),
	}
	if _, known := loginErrors[code]; known {
		if h := s.loginHelp(code, next); h != nil {
			d["Help"] = h
		}
	}
	return d
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	next := safeNext(r.PostFormValue("next"))
	if !s.Auth.TokenLogin() {
		http.Error(w, "token ile giriş kapalı: GitHub ile giriş yap", http.StatusNotFound)
		return
	}
	if !auth.SameOrigin(r) {
		http.Error(w, "başka siteden gelen giriş isteği reddedildi", http.StatusForbidden)
		return
	}
	if !s.Sessions.CheckToken(strings.TrimSpace(r.PostFormValue("token"))) {
		s.Log.Warn("web: failed login", "remote", r.RemoteAddr)
		s.render(w, r, http.StatusUnauthorized, "login", "Giriş yap", s.loginView(next, "bad_token"), loginErrors["bad_token"])
		return
	}
	s.Sessions.Issue(w, r)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// safeNext only allows same-site relative paths, so ?next= (and the return
// path kept in the OAuth cookie) cannot redirect off-site. Anything else
// becomes "/". Refused:
//
//   - absolute and scheme-relative URLs ("https://x", "//x", "/\x"),
//   - backslashes and control characters anywhere: browsers read "\" as
//     "/" and drop tabs and newlines, so "/\t/evil" would become "//evil",
//   - values that do not parse as a plain path with an optional query.
func safeNext(next string) string {
	if len(next) > 2048 || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.ContainsAny(next, "\\") || strings.IndexFunc(next, isControl) >= 0 {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" ||
		!strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "/"
	}
	return next
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }
