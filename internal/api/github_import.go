package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 15: importing a repository the GitHub App is installed on. A team
// claims an installation through the setup URL (internal/web/github.go);
// the repositories of its claimed installations are offered for import,
// and an import creates the app and queues the first deployment.

// RepoInspector reads repositories through the GitHub App's installation
// tokens (internal/github). Nil means no GitHub App is configured.
type RepoInspector interface {
	// BranchHead is the commit SHA branch points at.
	BranchHead(ctx context.Context, repo, branch string) (string, error)
	DefaultBranch(ctx context.Context, repo string) (string, error)
}

// ImportableView is a granted repository with the team that may import it:
// the team that claimed the installation granting it.
type ImportableView struct {
	store.ImportableRepo
	Team string `json:"team"`
	// CanImport: the caller is member or owner of Team.
	CanImport bool `json:"can_import"`
}

// ImportableRepos lists the repositories p's teams may import, by name.
func ImportableRepos(ctx context.Context, st *store.Store, a *auth.Authenticator, p auth.Principal) ([]ImportableView, error) {
	teams, err := a.Teams(ctx, p)
	if err != nil {
		return nil, err
	}
	out := []ImportableView{}
	for _, t := range teams {
		repos, err := st.ImportableRepos(ctx, []int64{t.ID})
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			out = append(out, ImportableView{ImportableRepo: r, Team: t.Slug,
				CanImport: store.RoleAllows(t.Role, store.RoleMember)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].FullName) < strings.ToLower(out[j].FullName)
	})
	return out, nil
}

// ImportRequest is POST /api/apps/import and the web UI's import form.
type ImportRequest struct {
	Repo string `json:"repo"`
	// Name defaults to one derived from the repository (DeriveAppName).
	Name string `json:"name"`
	// Team slug; empty picks the caller's team that may import the repo.
	Team string `json:"team"`
	// ProductionBranch defaults to the repository's default branch.
	ProductionBranch string `json:"production_branch"`
}

// ImportResult is a created app and its first deployment (nil when no
// GitHub App is configured, or its head commit could not be read: Warning).
type ImportResult struct {
	App        store.App
	Deployment *store.Deployment
	Warning    string
}

// githubTimeout bounds each GitHub call of an import.
const githubTimeout = 15 * time.Second

// ImportApp creates an app from a repository one of p's teams may import
// and queues the first deployment of its production branch head. Errors
// are user-facing messages; status is the HTTP status to answer with
// (500: err is internal and must not be shown).
func ImportApp(ctx context.Context, st *store.Store, a *auth.Authenticator, gh RepoInspector, log *slog.Logger,
	p auth.Principal, req ImportRequest) (ImportResult, int, error) {
	var res ImportResult
	repo := strings.TrimSpace(req.Repo)
	if !repoRe.MatchString(repo) {
		return res, http.StatusBadRequest, errors.New(`repo must look like "owner/repo"`)
	}
	list, err := ImportableRepos(ctx, st, a, p)
	if err != nil {
		return res, http.StatusInternalServerError, err
	}
	var matches []ImportableView
	for _, r := range list {
		if strings.EqualFold(r.FullName, repo) {
			matches = append(matches, r)
		}
	}
	notImportable := errors.New("repository not found among the GitHub App installations of your teams; " +
		"install the GitHub App on it first")

	var pick *ImportableView
	if slug := strings.TrimSpace(req.Team); slug != "" {
		t, err := a.TeamForApps(ctx, p, slug)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return res, http.StatusNotFound, errors.New("team not found")
		case errors.Is(err, auth.ErrForbidden):
			return res, http.StatusForbidden, errors.New("importing apps needs the member role on the team")
		case err != nil:
			return res, http.StatusInternalServerError, err
		}
		for i := range matches {
			if matches[i].Team == t.Slug {
				pick = &matches[i]
			}
		}
	} else {
		for i := range matches {
			if matches[i].CanImport {
				pick = &matches[i]
				break
			}
		}
		if pick == nil && len(matches) > 0 {
			return res, http.StatusForbidden, errors.New("importing apps needs the member role on the team")
		}
	}
	if pick == nil {
		return res, http.StatusNotFound, notImportable
	}
	if pick.App != "" {
		return res, http.StatusConflict, errors.New("this repository is already deployed as app " + pick.App)
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = DeriveAppName(pick.FullName)
	}
	if !naming.ValidAppName(name) {
		return res, http.StatusBadRequest,
			errors.New("name must be 2-31 chars: lowercase letters, digits, '-', starting with a letter")
	}
	branch := strings.TrimSpace(req.ProductionBranch)
	if branch == "" && gh != nil {
		cctx, cancel := context.WithTimeout(ctx, githubTimeout)
		branch, err = gh.DefaultBranch(cctx, pick.FullName)
		cancel()
		if err != nil {
			log.Warn("import: default branch", "repo", pick.FullName, "err", err)
			branch = ""
		}
	}
	if branch == "" {
		branch = "main"
	}
	if strings.ContainsAny(branch, " \t\r\n~^:?*[\\") || len(branch) > 255 {
		return res, http.StatusBadRequest, errors.New("invalid branch name")
	}

	team, err := st.GetTeamBySlug(ctx, pick.Team)
	if err != nil {
		return res, http.StatusInternalServerError, err
	}
	// GitHub's spelling of the name, so webhooks and tokens match it exactly.
	res.App, err = st.CreateAppInTeam(ctx, team.ID, name, pick.FullName, branch)
	if errors.Is(err, store.ErrConflict) {
		return res, http.StatusConflict, errors.New("an app with this name or repo already exists")
	}
	if err != nil {
		return res, http.StatusInternalServerError, err
	}
	log.Info("app imported", "app", name, "repo", pick.FullName, "team", team.Slug, "by", p.Login)
	if gh == nil {
		return res, http.StatusCreated, nil
	}

	// First deployment: the same queue entry a push webhook creates; the
	// worker gives it the production alias as usual.
	cctx, cancel := context.WithTimeout(ctx, githubTimeout)
	sha, err := gh.BranchHead(cctx, pick.FullName, branch)
	cancel()
	if err != nil {
		log.Warn("import: branch head", "repo", pick.FullName, "branch", branch, "err", err)
		res.Warning = "app created, but reading the head of " + branch + " failed; push to it to deploy"
		return res, http.StatusCreated, nil
	}
	d, _, err := st.EnqueueDeployment(ctx, res.App.ID, sha, branch, "Imported from GitHub")
	if err != nil {
		return res, http.StatusInternalServerError, err
	}
	log.Info("deployment queued", "app", name, "branch", branch, "sha", naming.ShortSHA(sha),
		"deployment", d.ID, "via", "import")
	res.Deployment = &d
	return res, http.StatusCreated, nil
}

// DeriveAppName turns a repository's name into a valid app name:
// "owner/My_Site.v2" → "my-site-v2", "owner/1password" → "app-1password".
// It returns "" when nothing valid remains.
func DeriveAppName(repo string) string {
	_, name, _ := strings.Cut(repo, "/")
	n := naming.Slug(name)
	if len(n) < 2 || n[0] < 'a' || n[0] > 'z' {
		n = "app-" + n
	}
	if len(n) > 31 {
		n = n[:31]
	}
	n = strings.TrimRight(n, "-")
	if !naming.ValidAppName(n) {
		return ""
	}
	return n
}

// ---- handlers ----

func (s *Server) listImportable(w http.ResponseWriter, r *http.Request) {
	list, err := ImportableRepos(r.Context(), s.Store, s.Auth, principal(r))
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type importView struct {
	App        appView         `json:"app"`
	Deployment *deploymentView `json:"deployment,omitempty"`
	Warning    string          `json:"warning,omitempty"`
}

func (s *Server) importApp(w http.ResponseWriter, r *http.Request) {
	var req ImportRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, status, err := ImportApp(r.Context(), s.Store, s.Auth, s.GitHubApp, s.Log, principal(r), req)
	if status == http.StatusInternalServerError {
		s.internalError(w, err)
		return
	}
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	out := importView{App: s.appView(res.App, nil), Warning: res.Warning}
	if res.Deployment != nil {
		v := s.deploymentView(*res.Deployment)
		out.Deployment = &v
	}
	writeJSON(w, http.StatusCreated, out)
}
