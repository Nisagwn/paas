package web

import (
	"html"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/build"
	"github.com/nisagwn/paas/internal/store"
)

// Navigation and first use: the header (sections, team switcher, user
// menu), the template helpers every page can use (when, crumbs, copy,
// framework) and the selected team of the dashboard.

// navData is the header of a signed-in page.
type navData struct {
	// Section is the highlighted top-level item: projects, teams, usage,
	// admin or tokens ("" for none).
	Section string
	// Teams are the caller's teams; the switcher is shown for two or more.
	Teams []store.Team
	// Team is the selected team (nil: all teams).
	Team *store.Team
	// Initial is the avatar letter of the user menu.
	Initial string
}

// sectionOf maps a path to its top-level section.
func sectionOf(path string) string {
	switch {
	case path == "/", path == "/import", strings.HasPrefix(path, "/apps/"), strings.HasPrefix(path, "/deployments/"),
		strings.HasPrefix(path, "/github/"):
		return "projects"
	case path == "/teams", strings.HasPrefix(path, "/teams/"):
		return "teams"
	case path == "/usage":
		return "usage"
	case path == "/admin", strings.HasPrefix(path, "/admin/"):
		return "admin"
	case path == "/tokens":
		return "tokens"
	}
	return ""
}

// teamCookie remembers the team selected in the switcher.
const teamCookie = "paas_team"

// selectedSlug is the team chosen with ?team= on the dashboard, else the
// remembered one. "" means all teams.
func selectedSlug(r *http.Request) string {
	if r.URL.Path == "/" && r.URL.Query().Has("team") {
		return r.URL.Query().Get("team")
	}
	if c, err := r.Cookie(teamCookie); err == nil {
		return c.Value
	}
	return ""
}

// rememberTeam stores (or clears) the switcher's choice when the dashboard
// is opened with ?team=.
func rememberTeam(w http.ResponseWriter, r *http.Request) {
	if !r.URL.Query().Has("team") {
		return
	}
	slug := r.URL.Query().Get("team")
	c := &http.Cookie{Name: teamCookie, Value: slug, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: auth.IsTLS(r), MaxAge: 365 * 24 * 3600}
	if slug == "" || !validSlug(slug) {
		c.Value, c.MaxAge = "", -1
	}
	http.SetCookie(w, c)
}

func validSlug(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// pickTeam returns the team of teams whose slug is slug, or nil.
func pickTeam(teams []store.Team, slug string) *store.Team {
	if slug == "" {
		return nil
	}
	for i := range teams {
		if teams[i].Slug == slug {
			return &teams[i]
		}
	}
	return nil
}

// nav builds the header data of a signed-in page.
func (s *Server) nav(r *http.Request, p auth.Principal) navData {
	n := navData{Section: sectionOf(r.URL.Path), Initial: initial(p)}
	teams, err := s.Auth.Teams(r.Context(), p)
	if err != nil {
		s.Log.Warn("web: header teams", "err", err)
		return n
	}
	n.Teams = teams
	n.Team = pickTeam(teams, selectedSlug(r))
	return n
}

// initial is the avatar letter of a principal.
func initial(p auth.Principal) string {
	name := p.Name
	if name == "" {
		name = p.Login
	}
	for _, c := range name {
		return strings.ToUpper(string(c))
	}
	return "?"
}

// ---- template helpers ----

func init() {
	funcs["when"] = when
	funcs["crumbs"] = crumbs
	funcs["copy"] = copyButton
	funcs["framework"] = frameworkName
	funcs["teamtabs"] = teamTabs
	funcs["join"] = strings.Join
	funcs["add1"] = func(i int) int { return i + 1 }
	funcs["initialOf"] = func(s string) string { return initial(auth.Principal{Login: s}) }
	funcs["trimScheme"] = func(u string) string {
		return strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	}
}

// when renders a relative time ("2 sa önce") whose tooltip and datetime
// carry the exact time; the layout script keeps the text current. It takes
// a time.Time or *time.Time (nil renders "—").
func when(v any) template.HTML {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return "—"
		}
		t = *x
	default:
		return "—"
	}
	if t.IsZero() {
		return "—"
	}
	exact := t.UTC().Format("02.01.2006 15:04:05 UTC")
	return template.HTML(`<time datetime="` + t.UTC().Format(time.RFC3339) + `" title="` + exact + `" data-ago>` +
		html.EscapeString(ago(t)) + `</time>`)
}

// crumbs renders a breadcrumb trail from label/href pairs; a final label
// without href is the current page:
//
//	{{crumbs "Ekip" "/teams/web" "blog" "/apps/blog" "Deploy #12"}}
func crumbs(parts ...string) template.HTML {
	var b strings.Builder
	b.WriteString(`<nav class="crumbs" aria-label="Konum"><ol>`)
	for i := 0; i < len(parts); i += 2 {
		label := html.EscapeString(parts[i])
		b.WriteString("<li>")
		if i+1 < len(parts) && parts[i+1] != "" {
			b.WriteString(`<a href="` + html.EscapeString(safeHref(parts[i+1])) + `">` + label + `</a>`)
		} else {
			b.WriteString(`<span aria-current="page">` + label + `</span>`)
		}
		b.WriteString("</li>")
	}
	b.WriteString("</ol></nav>")
	return template.HTML(b.String())
}

// safeHref keeps crumbs to local paths.
func safeHref(h string) string {
	if strings.HasPrefix(h, "/") && !strings.HasPrefix(h, "//") {
		return h
	}
	return "/"
}

// copyButton renders a copy-to-clipboard button for text. Without
// JavaScript it is hidden (the text itself stays selectable).
//
//	{{copy .URL}}  or  {{copy .URL "Adresi kopyala"}}
func copyButton(text string, label ...string) template.HTML {
	l := "Kopyala"
	if len(label) > 0 && label[0] != "" {
		l = label[0]
	}
	return template.HTML(`<button type="button" class="copy icon" data-copy="` + html.EscapeString(text) +
		`" aria-label="` + html.EscapeString(l) + `" title="` + html.EscapeString(l) + `">` + copyIcon + `</button>`)
}

// copyIcon is the inline SVG of the copy button (two sheets).
const copyIcon = `<svg aria-hidden="true" viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5">` +
	`<rect x="5" y="5" width="9" height="9" rx="2"/><path d="M11 5V3.5A1.5 1.5 0 0 0 9.5 2h-6A1.5 1.5 0 0 0 2 3.5v6A1.5 1.5 0 0 0 3.5 11H5"/></svg>`

// frameworkName is the display name of a framework id ("" stays "").
func frameworkName(id string) string {
	if id == "" {
		return ""
	}
	if n := build.FrameworkName(id); n != "" {
		return n
	}
	return id
}

// teamTabs renders the sections of a team page (Genel, Bildirimler, Plan ve
// kullanım); on is "general", "notifications" or "usage":
//
//	{{teamtabs $t.Slug "general"}}
func teamTabs(slug, on string) template.HTML {
	s := url.PathEscape(slug)
	tabs := []struct{ id, href, label string }{
		{"general", "/teams/" + s, teamTabGeneral},
		{"notifications", "/teams/" + s + "/notifications", teamTabNotifications},
		{"usage", "/usage?team=" + url.QueryEscape(slug), teamTabUsage},
	}
	var b strings.Builder
	b.WriteString(`<nav class="tabs tabs-spaced" aria-label="` + teamTabsLabel + `">`)
	for _, t := range tabs {
		b.WriteString("<a ")
		if t.id == on {
			b.WriteString(`class="on" aria-current="page" `)
		}
		b.WriteString(`href="` + html.EscapeString(t.href) + `">` + t.label + "</a>")
	}
	b.WriteString("</nav>")
	return template.HTML(b.String())
}
