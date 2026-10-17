package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/naming"
)

// The landing page (karşılama sayfası): GET / for visitors without a
// session; signed-in callers get the dashboard.
//
// Everything on it comes from the server's configuration: the platform
// domain in the example addresses, whether plan limits apply (Faz 23) and
// whether anyone may sign up (PAAS_OPEN_SIGNUP). The plan limits
// themselves are listed on /signup.

// repoURL is the source repository, linked from the landing page.
const repoURL = "https://github.com/Nisagwn/paas"

// openSignup reports whether any GitHub account may create an account here.
func (s *Server) openSignup() bool { return s.GitHub != nil && s.GitHub.OpenSignup }

// home serves / : the landing page to visitors without a session, the
// dashboard (through authed) to everyone else. A bearer header or a
// session that cannot be read (suspended, store error) goes to the
// dashboard handler, which answers it as before.
func (s *Server) home(dashboard http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if _, err := s.Auth.Session(r); errors.Is(err, auth.ErrUnauthenticated) {
				s.landingPage(w, r)
				return
			}
		}
		dashboard.ServeHTTP(w, r)
	}
}

func (s *Server) landingPage(w http.ResponseWriter, r *http.Request) {
	domain := s.Domain
	if domain == "" {
		domain = "localtest.me"
	}
	host := func(name string) string { return naming.URL(s.Scheme, name+"."+domain) }
	s.render(w, r, http.StatusOK, "landing", "Git push'la, saniyeler içinde canlıda", map[string]any{
		"AppURL":     host("blog"),
		"PreviewURL": host("yeni-tasarim-blog"),
		"CommitURL":  host("3f9c2a1-blog"),
		"Signup":     s.openSignup(),
		"Enforced":   s.Plans != nil,
		"PublicURL":  strings.TrimRight(s.PublicURL, "/"),
		"Repo":       repoURL,
	}, "")
}
