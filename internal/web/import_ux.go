package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/build"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/store"
)

// "Yeni proje" (/import): a three-step flow — connect GitHub, pick a
// repository, configure and deploy — plus adding a repository by hand.
// Every step is a plain page: links and forms work without JavaScript.

type importRow struct {
	api.ImportableView
	// Suggested app name.
	Name string
}

type importGroup struct {
	Account string
	Repos   []importRow
}

// manualForm is the "add by hand" form (POST /apps).
type manualForm struct {
	Name, Repo, Branch, Team string
	Open                     bool
}

// importConfig is step 3: the chosen repository and its settings.
type importConfig struct {
	Repo importRow
	// Teams that may import the repository (member+); Team is selected.
	Teams []store.Team
	Team  string
	Name  string
	// Branch is the typed production branch; DefaultBranch the
	// repository's default (placeholder), "" when unknown.
	Branch, DefaultBranch string
}

// errorHelp tells what to do about an error: a sentence and a link.
type errorHelp struct {
	Text, Href, Label string
}

// importState carries a failed submission back into the page.
type importState struct {
	Manual manualForm
	Error  string
	Help   *errorHelp
	// Config is set when the import form (step 3) failed.
	Config *importConfig
}

func (s *Server) importPage(w http.ResponseWriter, r *http.Request) {
	s.renderImport(w, r, http.StatusOK, importState{})
}

func (s *Server) renderImport(w http.ResponseWriter, r *http.Request, status int, st importState) {
	ctx := r.Context()
	p := me(r)
	list, err := api.ImportableRepos(ctx, s.Store, s.Auth, p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	teams, err := s.Auth.Teams(ctx, p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var writable []store.Team
	for _, t := range teams {
		if store.RoleAllows(t.Role, store.RoleMember) {
			writable = append(writable, t)
		}
	}
	selected := pickTeam(teams, selectedSlug(r))

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var groups []importGroup
	total := 0
	for _, v := range list {
		total++
		if q != "" && !strings.Contains(strings.ToLower(v.FullName), strings.ToLower(q)) {
			continue
		}
		// Repositories come sorted by full name, so one account's are adjacent.
		if len(groups) == 0 || !strings.EqualFold(groups[len(groups)-1].Account, v.AccountLogin) {
			groups = append(groups, importGroup{Account: v.AccountLogin})
		}
		g := &groups[len(groups)-1]
		g.Repos = append(g.Repos, importRow{ImportableView: v, Name: api.DeriveAppName(v.FullName)})
	}

	// Step 3: ?repo= (from the list) or a failed import.
	cfg := st.Config
	if cfg == nil {
		if repo := strings.TrimSpace(r.URL.Query().Get("repo")); repo != "" {
			cfg = &importConfig{Repo: importRow{ImportableView: api.ImportableView{}}, Team: r.URL.Query().Get("team")}
			cfg.Repo.FullName = repo
		}
	}
	if cfg != nil {
		repo := cfg.Repo.FullName
		cfg = s.fillConfig(ctx, cfg, list, selected)
		if cfg == nil && st.Error == "" {
			st.Error, st.Help = importRepoMissing, &helpInstall
			for _, v := range list {
				if strings.EqualFold(v.FullName, repo) && v.App != "" {
					st.Error = importRepoExisting(v.App)
					st.Help = &errorHelp{Text: helpExisting.Text, Href: "/apps/" + v.App, Label: helpExisting.Label}
				}
			}
		}
	}

	step := 1
	switch {
	case cfg != nil:
		step = 3
	case total > 0:
		step = 2
	}
	if st.Manual.Branch == "" {
		st.Manual.Branch = "main"
	}
	if st.Manual.Team == "" && selected != nil {
		st.Manual.Team = selected.Slug
	}
	configured := s.GitHubAppSlug != ""
	if !configured {
		st.Manual.Open = true
	}
	frameworks := make([]string, 0, len(build.Frameworks))
	for _, f := range build.Frameworks {
		frameworks = append(frameworks, f.Name)
	}
	installEnabled := s.installEnabled() && p.UserID != 0
	s.render(w, r, status, "import", "Yeni proje", map[string]any{
		"Groups": groups, "Total": total, "Query": q, "Teams": writable, "MultiTeam": len(teams) > 1,
		"Configured": configured, "InstallEnabled": installEnabled,
		"GitHubLogin": s.GitHub != nil, "Deploys": s.GitHubApp != nil,
		"Poll": configured && installEnabled && total == 0,
		"Step": step, "Config": cfg, "Manual": st.Manual, "Error": tr(st.Error), "Help": st.Help,
		"Frameworks": frameworks, "Domain": s.Domain, "Scheme": scheme(s.Scheme),
	}, "")
}

// fillConfig completes step 3 from the importable list; nil when the
// repository is not importable by the caller (or already a project).
func (s *Server) fillConfig(ctx context.Context, cfg *importConfig, list []api.ImportableView, selected *store.Team) *importConfig {
	var teams []store.Team
	var row *api.ImportableView
	for i := range list {
		v := list[i]
		if !strings.EqualFold(v.FullName, cfg.Repo.FullName) {
			continue
		}
		if v.App != "" {
			return nil
		}
		if !v.CanImport {
			continue
		}
		if row == nil {
			row = &list[i]
		}
		teams = append(teams, store.Team{Slug: v.Team, Name: v.Team})
	}
	if row == nil {
		return nil
	}
	out := *cfg
	out.Repo = importRow{ImportableView: *row, Name: api.DeriveAppName(row.FullName)}
	out.Teams = s.teamNames(ctx, teams)
	if pickTeam(out.Teams, out.Team) == nil {
		out.Team = out.Teams[0].Slug
		if selected != nil && pickTeam(out.Teams, selected.Slug) != nil {
			out.Team = selected.Slug
		}
	}
	if out.Name == "" {
		out.Name = out.Repo.Name
	}
	if s.GitHubApp != nil {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if b, err := s.GitHubApp.DefaultBranch(cctx, row.FullName); err == nil {
			out.DefaultBranch = b
		}
		cancel()
	}
	return &out
}

// teamNames fills in the display names of teams known only by slug.
func (s *Server) teamNames(ctx context.Context, teams []store.Team) []store.Team {
	for i := range teams {
		if t, err := s.Store.GetTeamBySlug(ctx, teams[i].Slug); err == nil {
			teams[i].Name = t.Name
		}
	}
	return teams
}

func (s *Server) importApp(w http.ResponseWriter, r *http.Request) {
	if !s.allowImport(w, r) {
		return
	}
	req := api.ImportRequest{
		Repo: r.PostFormValue("repo"), Name: r.PostFormValue("name"),
		Team: r.PostFormValue("team"), ProductionBranch: r.PostFormValue("production_branch"),
	}
	res, status, err := api.ImportAppLimited(r.Context(), s.Store, s.Auth, s.GitHubApp, s.Plans, s.Log, me(r), req)
	if status == http.StatusInternalServerError {
		s.internalError(w, r, err)
		return
	}
	if err != nil {
		cfg := &importConfig{Name: strings.TrimSpace(req.Name), Team: req.Team, Branch: strings.TrimSpace(req.ProductionBranch)}
		cfg.Repo.FullName = strings.TrimSpace(req.Repo)
		s.renderImport(w, r, status, importState{Config: cfg, Error: tr(err.Error()), Help: importHelp(err, status)})
		return
	}
	if res.Deployment != nil {
		redirect(w, r, "/apps/"+res.App.Name+"?ok=imported")
		return
	}
	if strings.Contains(res.Warning, "plan limit reached") { // Faz 23
		redirect(w, r, "/apps/"+res.App.Name+"?ok=imported-limit")
		return
	}
	redirect(w, r, "/apps/"+res.App.Name+"?ok=imported-idle")
}

// importHelp picks what to do about an import error.
func importHelp(err error, status int) *errorHelp {
	msg := err.Error()
	switch {
	case plans.AsLimit(err) != nil:
		return &helpLimit
	case strings.HasPrefix(msg, "this repository is already deployed as app "):
		app := strings.TrimPrefix(msg, "this repository is already deployed as app ")
		if naming.ValidAppName(app) {
			return &errorHelp{Text: helpExisting.Text, Href: "/apps/" + app, Label: helpExisting.Label}
		}
	case status == http.StatusConflict:
		return &helpName
	case status == http.StatusForbidden:
		return &helpRole
	case status == http.StatusNotFound:
		return &helpInstall
	}
	return nil
}

// scheme is the scheme of app links ("https" when unset).
func scheme(s string) string {
	if s == "" {
		return "https"
	}
	return s
}
