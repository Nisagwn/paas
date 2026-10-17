package web

import (
	"net/http"

	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/store"
)

// Sign-up page (/signup). Signing up is the same GitHub authorization as
// signing in (oauth.go): with PAAS_OPEN_SIGNUP the first sign-in creates a
// personal team on the Hobby plan; otherwise only admins, allowlisted users
// or organization members and invited users get in. The page says which
// case applies and what happens to the account and the code.

// signupView is the data of templates/signup.html.
type signupView struct {
	GitHub     bool // GitHub sign-in configured
	OpenSignup bool
	TokenLogin bool // dev mode: the token form on /login
	Next       string
	// PlanName and Plan are the limits of a new personal team (nil when no
	// plan is enforced).
	PlanName string
	Plan     []string
	// MinAge is the minimum GitHub account age of open sign-up ("30 gün").
	MinAge string
	// App: repositories are connected through the GitHub App.
	App bool
	// OrgCheck: sign-in asks for read:org; Orgs: allowlisted organizations exist.
	OrgCheck, Orgs bool
}

func (s *Server) signupPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if s.signedIn(r) {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	v := signupView{GitHub: s.GitHub != nil, TokenLogin: s.Auth.TokenLogin(), App: s.GitHubAppSlug != "", Next: next}
	if g := s.GitHub; g != nil {
		v.OpenSignup = g.OpenSignup
		v.OrgCheck = g.Scope() != ""
		v.Orgs = len(g.AllowedOrgs) > 0
		if g.OpenSignup && g.MinAccountAge > 0 {
			v.MinAge = ageLabel(g.MinAccountAge)
		}
	}
	if v.Next == "/" && v.App && v.OpenSignup {
		// A new account starts at importing its first repository; the
		// dashboard's checklist leads there too. Invited users land on
		// their team's projects.
		v.Next = "/import"
	}
	if s.Plans != nil && v.OpenSignup {
		if l, ok := s.Plans.Catalog[store.PlanHobby]; ok {
			v.PlanName = planLabels[store.PlanHobby]
			v.Plan = planFacts(l)
		}
	}
	s.render(w, r, http.StatusOK, "signup", "Kaydol", v, "")
}

// planFacts lists the headline limits of a plan in Turkish.
func planFacts(l plans.Limits) []string {
	f := func(n int, prefix, unit string) string {
		if n < 0 {
			return "sınırsız " + unit
		}
		return prefix + number(int64(n)) + " " + unit
	}
	return []string{
		f(l.Projects, "", "proje"),
		f(l.BuildMinutes, "ayda ", "build dakikası"),
		f(l.DeploymentsPerDay, "günde ", "deploy"),
		f(l.Domains, "", "özel alan adı"),
		f(l.Addons, "", "veritabanı"),
	}
}
