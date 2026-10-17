package web

import (
	"embed"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/plans"
)

// The landing page (karşılama sayfası):
//
//	GET /                    visitors without a session; signed-in callers get the dashboard
//	GET /static/mimari.svg   the architecture diagram of docs/MIMARI.md
//
// Everything on it comes from the server's configuration: the platform
// domain in the example addresses, the plan limits (Faz 23) and whether
// anyone may sign up (PAAS_OPEN_SIGNUP).

// static/mimari.svg is a copy of docs/images/mimari-genel.svg (docs/ does
// not reach the Docker build); TestArchitectureImageInSync keeps them equal.
//
//go:embed static/mimari.svg
var staticFS embed.FS

// repoURL is the source repository, linked from the landing page.
const repoURL = "https://github.com/Nisagwn/paas"

// landingPlan is one plan card.
type landingPlan struct {
	Name  string // plan id: hobby, pro, unlimited
	Label string
	Note  string
	Rows  [][2]string
}

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
		"Domain":     domain,
		"AppURL":     host("blog"),
		"PreviewURL": host("yeni-tasarim-blog"),
		"CommitURL":  host("3f9c2a1-blog"),
		"Signup":     s.openSignup(),
		"GitHub":     s.GitHub != nil,
		"Enforced":   s.Plans != nil,
		"Plans":      s.landingPlans(),
		"PublicURL":  strings.TrimRight(s.PublicURL, "/"),
		"Repo":       repoURL,
	}, "")
}

// landingPlans lists the configured plans, cheapest first.
func (s *Server) landingPlans() []landingPlan {
	catalog := plans.DefaultCatalog()
	if s.Plans != nil && s.Plans.Catalog != nil {
		catalog = s.Plans.Catalog
	}
	notes := map[string]string{
		"hobby":     "Kişisel projeler ve yan işler için. Her yeni ekip bu planla başlar.",
		"pro":       "Ürününü burada çalıştıran ekipler için. Platform yöneticisi atar.",
		"unlimited": "Kurum içi kurulumlar için; yalnızca küme kotası sınırlar. Platform yöneticisi atar.",
	}
	n := func(v int, unit string) string {
		if v < 0 {
			return "Sınırsız"
		}
		return number(int64(v)) + unit
	}
	// q shows a Kubernetes quantity: "2" → "2 çekirdek", "2Gi" → "2 GiB".
	q := func(v, unit string) string {
		if v == "" {
			return "Küme kotası"
		}
		if _, err := strconv.Atoi(v); err == nil {
			return v + unit
		}
		for _, sfx := range []string{"Gi", "Mi", "Ti"} {
			if n, ok := strings.CutSuffix(v, sfx); ok {
				return n + " " + sfx + "B"
			}
		}
		return v
	}
	out := make([]landingPlan, 0, len(plans.Plans))
	for _, name := range plans.Plans {
		l, ok := catalog[name]
		if !ok {
			continue
		}
		timeout := n(l.BuildTimeout, " dk")
		if l.BuildTimeout < 0 {
			timeout = "Yok"
		}
		out = append(out, landingPlan{Name: name, Label: planLabel(name), Note: notes[name], Rows: [][2]string{
			{"Proje", n(l.Projects, "")},
			{"Aylık build dakikası", n(l.BuildMinutes, " dk")},
			{"Günlük deploy", n(l.DeploymentsPerDay, "")},
			{"Eşzamanlı build", n(l.ConcurrentBuilds, "")},
			{"Özel alan adı", n(l.Domains, "")},
			{"Veritabanı", n(l.Addons, "")},
			{"Süreç başına kopya", n(l.Replicas, "")},
			{"Cron işi", n(l.Crons, "")},
			{"Build zaman aşımı", timeout},
			{"Proje başına CPU", q(l.CPU, " çekirdek")},
			{"Proje başına bellek", q(l.Memory, "")},
		}})
	}
	return out
}

// architectureSVG serves the architecture diagram shown on the landing page.
func architectureSVG(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/mimari.svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/svg+xml")
	h.Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}
