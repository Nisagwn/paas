package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 15: installing the GitHub App, linking the installation to a team,
// and importing its repositories.
//
// Install and claim:
//
//  1. /github/install?team=<slug> (member+) signs {team, user} into a state
//     valid for 30 minutes and sends the browser to
//     github.com/apps/<slug>/installations/new?state=…
//  2. The user picks repositories; GitHub redirects to the App's Setup URL,
//     /github/setup?installation_id=…&setup_action=install|update&state=….
//     That is a cross-site navigation, so the SameSite=Strict session cookie
//     is not sent; the signed state says who started it and for which team.
//  3. Knowing an installation id proves nothing, so /github/setup runs the
//     GitHub OAuth authorization (PKCE, same App, no prompt when the user has
//     signed in with it before). The OAuth state, PKCE verifier, installation,
//     team and user ride in a signed 10-minute cookie (paas_ghclaim).
//  4. /auth/github/callback finds that cookie, exchanges the code, and claims
//     the installation only when the token's GitHub account is the user from
//     the state, GET /user/installations lists the installation, and the user
//     is still member+ of the team. If the installation webhook has not
//     arrived yet, the row is created from GitHub's answer.
//
// A Setup URL visit without a valid state (GitHub's own "Configure" page with
// "Redirect on update") changes nothing: repository changes arrive as
// installation_repositories webhooks.

const (
	installStatePurpose = "github-install"
	installStateTTL     = 30 * time.Minute
	claimPurpose        = "github-claim"
	claimCookie         = "paas_ghclaim"
	claimTTL            = 10 * time.Minute
)

// installEnabled reports whether the install flow can run: an App slug and
// GitHub sign-in (the claim needs a user token).
func (s *Server) installEnabled() bool { return s.GitHubAppSlug != "" && s.GitHub != nil }

// installURL is the App's install page with a signed state for team and user.
func (s *Server) installURL(teamID, userID int64) string {
	web := github.DefaultWebURL
	if s.GitHub != nil && s.GitHub.App != nil && s.GitHub.App.WebURL != "" {
		web = strings.TrimRight(s.GitHub.App.WebURL, "/")
	}
	state := s.Sessions.Seal(installStatePurpose, fmt.Sprintf("%d.%d", teamID, userID), installStateTTL)
	return web + "/apps/" + url.PathEscape(s.GitHubAppSlug) + "/installations/new?state=" + url.QueryEscape(state)
}

// openInstallState returns the team and user of a valid install state.
func (s *Server) openInstallState(state string) (teamID, userID int64, ok bool) {
	v, ok := s.Sessions.Open(installStatePurpose, state)
	if !ok {
		return 0, 0, false
	}
	t, u, _ := strings.Cut(v, ".")
	teamID, err1 := strconv.ParseInt(t, 10, 64)
	userID, err2 := strconv.ParseInt(u, 10, 64)
	return teamID, userID, err1 == nil && err2 == nil && teamID > 0 && userID > 0
}

// githubInstall starts an installation for a team (member+).
func (s *Server) githubInstall(w http.ResponseWriter, r *http.Request) {
	if !s.installEnabled() {
		s.errorPage(w, r, http.StatusNotFound, "The GitHub App is not configured, or GitHub sign-in is off.")
		return
	}
	p := me(r)
	if p.UserID == 0 {
		s.errorPage(w, r, http.StatusForbidden, "Installing the GitHub App needs a GitHub sign-in, not the admin token.")
		return
	}
	team, err := s.Auth.TeamForApps(r.Context(), p, r.URL.Query().Get("team"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound, "Team not found.")
		return
	case errors.Is(err, auth.ErrForbidden):
		s.errorPage(w, r, http.StatusForbidden, "Installing the GitHub App needs the member role on the team.")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, s.installURL(team.ID, p.UserID), http.StatusFound)
}

// githubSetup is the App's Setup URL. It is not behind authed: the session
// cookie does not come along on GitHub's redirect.
func (s *Server) githubSetup(w http.ResponseWriter, r *http.Request) {
	if s.GitHub == nil {
		s.errorPage(w, r, http.StatusNotImplemented,
			"Linking a GitHub App installation needs GitHub sign-in (PAAS_GITHUB_OAUTH_CLIENT_ID and "+
				"PAAS_GITHUB_OAUTH_CLIENT_SECRET, the App's client credentials). In development mode, create apps "+
				"with New app and a repository webhook instead.")
		return
	}
	q := r.URL.Query()
	id, err := strconv.ParseInt(q.Get("installation_id"), 10, 64)
	if err != nil || id <= 0 {
		s.errorPage(w, r, http.StatusBadRequest, "GitHub did not send an installation id.")
		return
	}
	teamID, userID, ok := s.openInstallState(q.Get("state"))
	if !ok {
		if q.Get("setup_action") == "update" {
			s.render(w, r, http.StatusOK, "redirect", "GitHub App", map[string]string{
				"Next": "/import?ok=github-updated", "Message": "GitHub App settings saved."}, "")
			return
		}
		s.Log.Warn("web: github setup without a valid state", "installation", id, "remote", r.RemoteAddr)
		s.errorPage(w, r, http.StatusBadRequest,
			"This installation link expired or was not started here. Open Import and click Install GitHub App "+
				"again: GitHub shows the existing installation and sends you back to link it to your team.")
		return
	}
	state, verifier := randomString(), randomString()
	value := strings.Join([]string{state, verifier, strconv.FormatInt(id, 10),
		strconv.FormatInt(teamID, 10), strconv.FormatInt(userID, 10)}, " ")
	http.SetCookie(w, &http.Cookie{
		Name: claimCookie, Value: s.Sessions.Seal(claimPurpose, value, claimTTL),
		Path: "/auth/github", MaxAge: int(claimTTL.Seconds()), HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: auth.IsTLS(r),
	})
	http.Redirect(w, r, s.GitHub.App.AuthorizeURL(s.GitHub.RedirectURI, state, auth.PKCEChallenge(verifier), s.GitHub.Scope()),
		http.StatusFound)
}

// claimFromCookie returns the claim cookie's fields when it belongs to this
// callback (its OAuth state matches).
func (s *Server) claimFromCookie(r *http.Request) ([]string, bool) {
	c, err := r.Cookie(claimCookie)
	if err != nil {
		return nil, false
	}
	v, ok := s.Sessions.Open(claimPurpose, c.Value)
	if !ok {
		return nil, false
	}
	parts := strings.Split(v, " ")
	if len(parts) != 5 || subtle.ConstantTimeCompare([]byte(parts[0]), []byte(r.URL.Query().Get("state"))) != 1 {
		return nil, false
	}
	return parts, true
}

// claimMessages are the fixed failure messages of a claim.
var claimMessages = map[int]string{
	http.StatusUnauthorized: "GitHub authorization was cancelled; the installation is not linked.",
	http.StatusBadGateway:   "Checking the installation with GitHub failed. The server log has details.",
}

// claimCallback finishes step 4 of the install flow (see the top of the file).
func (s *Server) claimCallback(w http.ResponseWriter, r *http.Request, parts []string) {
	http.SetCookie(w, &http.Cookie{Name: claimCookie, Value: "", Path: "/auth/github", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: auth.IsTLS(r)})
	verifier := parts[1]
	id, _ := strconv.ParseInt(parts[2], 10, 64)
	teamID, _ := strconv.ParseInt(parts[3], 10, 64)
	userID, _ := strconv.ParseInt(parts[4], 10, 64)
	q := r.URL.Query()
	if q.Get("error") != "" || q.Get("code") == "" {
		s.errorPage(w, r, http.StatusUnauthorized, claimMessages[http.StatusUnauthorized])
		return
	}
	ctx := r.Context()
	status, msg, err := s.claim(ctx, q.Get("code"), verifier, id, teamID, userID)
	switch {
	case status == http.StatusInternalServerError:
		s.internalError(w, r, err)
		return
	case err != nil:
		s.Log.Warn("web: github installation claim refused", "installation", id, "team", teamID, "user", userID,
			"remote", r.RemoteAddr, "err", err)
		s.errorPage(w, r, status, msg)
		return
	}
	// Same-site hop, so the Strict session cookie is sent (see githubCallback).
	s.render(w, r, http.StatusOK, "redirect", "GitHub App", map[string]string{
		"Next": "/import?ok=github", "Message": "GitHub App installed."}, "")
}

// claim verifies and records the claim. msg is user-facing; status 500
// means err is internal.
func (s *Server) claim(ctx context.Context, code, verifier string, id, teamID, userID int64) (int, string, error) {
	u, err := s.Store.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusForbidden, "Your account no longer exists.", err
	}
	if err != nil {
		return http.StatusInternalServerError, "", err
	}
	need := func(teamID int64) (bool, error) {
		role, err := s.Store.TeamRole(ctx, userID, teamID)
		return store.RoleAllows(role, store.RoleMember), err
	}
	if ok, err := need(teamID); err != nil {
		return http.StatusInternalServerError, "", err
	} else if !ok {
		return http.StatusForbidden, "Linking a GitHub App installation needs the member role on the team.",
			errors.New("not a member of the team")
	}

	grant, err := s.GitHub.VerifyInstallation(ctx, code, verifier, u.GitHubID, id)
	switch {
	case errors.Is(err, auth.ErrWrongAccount):
		return http.StatusForbidden, "GitHub authorized a different account than the one signed in here. " +
			"Sign in to GitHub as @" + u.Login + " and try again.", err
	case errors.Is(err, auth.ErrNoInstallationAccess):
		return http.StatusForbidden, "Your GitHub account cannot access this installation.", err
	case err != nil:
		s.Log.Error("web: github installation check", "installation", id, "err", err)
		return http.StatusBadGateway, claimMessages[http.StatusBadGateway], err
	}

	in, err := s.Store.GetInstallation(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The installation webhook has not arrived yet: record what the
		// user token shows; the webhook (or a resync) completes it later.
		if err := s.Store.UpsertInstallation(ctx, grant.Installation); err != nil {
			return http.StatusInternalServerError, "", err
		}
		if err := s.Store.AddInstallationRepos(ctx, id, grant.Repos); err != nil {
			return http.StatusInternalServerError, "", err
		}
	case err != nil:
		return http.StatusInternalServerError, "", err
	case in.TeamID != nil && *in.TeamID != teamID:
		// Moving an installation away from a team needs member+ there too.
		if ok, err := need(*in.TeamID); err != nil {
			return http.StatusInternalServerError, "", err
		} else if !ok {
			return http.StatusConflict, "This installation is already linked to another team.",
				errors.New("claimed by another team")
		}
	}
	if err := s.Store.ClaimInstallation(ctx, id, teamID); err != nil {
		return http.StatusInternalServerError, "", err
	}
	s.Log.Info("web: github installation claimed", "installation", id, "account", grant.Installation.AccountLogin,
		"team", teamID, "by", u.Login)
	return http.StatusOK, "", nil
}

// ---- import page ----

type importRow struct {
	api.ImportableView
	// Suggested app name.
	Name string
}

type importGroup struct {
	Account string
	Repos   []importRow
}

func (s *Server) importPage(w http.ResponseWriter, r *http.Request) {
	s.renderImport(w, r, http.StatusOK, "")
}

func (s *Server) renderImport(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	p := me(r)
	list, err := api.ImportableRepos(r.Context(), s.Store, s.Auth, p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	teams, err := s.Auth.Teams(r.Context(), p)
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
	var groups []importGroup
	for _, v := range list {
		if len(groups) == 0 || !strings.EqualFold(groups[len(groups)-1].Account, v.AccountLogin) {
			groups = append(groups, importGroup{Account: v.AccountLogin})
		}
		g := &groups[len(groups)-1]
		g.Repos = append(g.Repos, importRow{ImportableView: v, Name: api.DeriveAppName(v.FullName)})
	}
	// Repositories come sorted by full name, so one account's are adjacent.
	s.render(w, r, status, "import", "Import from GitHub", map[string]any{
		"Groups": groups, "Teams": writable,
		"Configured": s.GitHubAppSlug != "", "InstallEnabled": s.installEnabled() && p.UserID != 0,
		"GitHubLogin": s.GitHub != nil, "Deploys": s.GitHubApp != nil,
	}, errMsg)
}

func (s *Server) importApp(w http.ResponseWriter, r *http.Request) {
	req := api.ImportRequest{
		Repo: r.PostFormValue("repo"), Name: r.PostFormValue("name"),
		Team: r.PostFormValue("team"), ProductionBranch: r.PostFormValue("production_branch"),
	}
	res, status, err := api.ImportApp(r.Context(), s.Store, s.Auth, s.GitHubApp, s.Log, me(r), req)
	if status == http.StatusInternalServerError {
		s.internalError(w, r, err)
		return
	}
	if err != nil {
		msg := err.Error()
		s.renderImport(w, r, status, strings.ToUpper(msg[:1])+msg[1:]+".")
		return
	}
	if res.Deployment != nil {
		redirect(w, r, "/apps/"+res.App.Name+"?ok=imported")
		return
	}
	redirect(w, r, "/apps/"+res.App.Name+"?ok=imported-idle")
}
