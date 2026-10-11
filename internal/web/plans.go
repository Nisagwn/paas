package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/ratelimit"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 23: plans, usage, the admin page, rate limits, suspension and the
// legal pages in the UI.
//
//	GET  /usage[?team=<slug>]                 usage of one of the caller's teams
//	GET  /admin                               platform admin: teams, plans, suspension
//	POST /admin/teams/{slug}/plan             platform admin
//	POST /admin/teams/{slug}/suspend          platform admin
//	POST /admin/teams/{slug}/unsuspend        platform admin
//	POST /admin/users/suspend                 platform admin
//	POST /admin/users/unsuspend               platform admin
//	GET  /terms, /privacy                     public

// Legal configures the legal pages (config.Public).
type Legal struct {
	// ContactEmail: PAAS_CONTACT_EMAIL; empty hides the address.
	ContactEmail string
	// Operator: who runs the platform (PAAS_OPERATOR_NAME).
	Operator string
}

// planLabels are the Turkish names of the team plans.
var planLabels = map[string]string{
	store.PlanHobby:     "Hobby",
	store.PlanPro:       "Pro",
	store.PlanUnlimited: "Sınırsız",
}

var planLabel = label(planLabels)

// ---- rate limits and suspension ----

func (s *Server) tooMany(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusTooManyRequests, "Çok fazla istek. Biraz bekleyip tekrar dene.")
}

// allowWrite applies the write rate limit to a state-changing form.
func (s *Server) allowWrite(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
	if s.Limits == nil {
		return true
	}
	if ok, wait := s.Limits.Allow(s.Limits.Writes, r, principalKey(p)); !ok {
		ratelimit.RetryAfter(w, wait)
		s.tooMany(w, r)
		return false
	}
	return true
}

// allowImport applies the import rate limit (new projects).
func (s *Server) allowImport(w http.ResponseWriter, r *http.Request) bool {
	if s.Limits == nil {
		return true
	}
	if ok, wait := s.Limits.Allow(s.Limits.Import, r, principalKey(me(r))); !ok {
		ratelimit.RetryAfter(w, wait)
		s.tooMany(w, r)
		return false
	}
	return true
}

// limitSignIn applies the sign-in rate limit per IP.
func (s *Server) limitSignIn(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Limits != nil {
			if ok, wait := s.Limits.Allow(s.Limits.SignIn, r, ""); !ok {
				ratelimit.RetryAfter(w, wait)
				s.render(w, r, http.StatusTooManyRequests, "login", "Giriş yap", s.loginData("/"),
					"Çok fazla giriş denemesi. Biraz bekleyip tekrar dene.")
				return
			}
		}
		h(w, r)
	}
}

func principalKey(p auth.Principal) string {
	if p.UserID != 0 {
		return strconv.FormatInt(p.UserID, 10)
	}
	if p.Admin {
		return "admin"
	}
	return ""
}

// suspendedSession ends the session of a suspended user.
func (s *Server) suspendedSession(w http.ResponseWriter, r *http.Request) {
	s.Sessions.Clear(w, r)
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		http.Redirect(w, r, "/login?error=suspended", http.StatusSeeOther)
		return
	}
	http.Error(w, "hesabın askıya alındı", http.StatusForbidden)
}

// limitError returns the status and message of a plan refusal.
func limitError(err error) (int, string, bool) {
	le := plans.AsLimit(err)
	if le == nil {
		return 0, "", false
	}
	return le.Status, le.Msg, true
}

// ---- usage ----

// usageBar is one row of the usage page.
type usageBar struct {
	Label string
	Used  int64
	Max   int // plans.Unlimited: no bound
	Unit  string
	Note  string
}

// Unlimited reports whether the bar has no bound.
func (b usageBar) Unlimited() bool { return b.Max < 0 }

// Percent is the filled share, 0-100.
func (b usageBar) Percent() int {
	if b.Max < 0 {
		return 0
	}
	if b.Max == 0 {
		if b.Used > 0 {
			return 100
		}
		return 0
	}
	p := int(b.Used * 100 / int64(b.Max))
	if p > 100 {
		p = 100
	}
	return p
}

// Level is the bar's color class: ok, warn (80%+) or full.
func (b usageBar) Level() string {
	switch {
	case b.Max < 0:
		return "ok"
	case b.Used >= int64(b.Max):
		return "full"
	case b.Percent() >= 80:
		return "warn"
	}
	return "ok"
}

func usageBars(st plans.TeamStatus) []usageBar {
	l, u := st.Limits, st.Usage
	return []usageBar{
		{Label: "Projeler", Used: int64(u.Projects), Max: l.Projects},
		{Label: "Build dakikası (bu ay)", Used: st.BuildMinutes, Max: l.BuildMinutes, Unit: "dk",
			Note: "Her ayın ilk günü (UTC) sıfırlanır."},
		{Label: "Deploy (bugün)", Used: int64(u.DeploymentsToday), Max: l.DeploymentsPerDay,
			Note: "Her gün 00:00 UTC'de sıfırlanır."},
		{Label: "Veritabanları", Used: int64(u.Addons), Max: l.Addons},
		{Label: "Alan adları", Used: int64(u.Domains), Max: l.Domains},
		{Label: "Eşzamanlı build", Used: int64(u.ActiveBuilds), Max: l.ConcurrentBuilds,
			Note: "Sınırdayken yeni build'ler sırada bekler; diğer ekipleri engellemez."},
	}
}

// limitRows are the remaining limits, shown as text.
func limitRows(l plans.Limits) [][2]string {
	n := func(v int, unit string) string {
		if v < 0 {
			return "Sınırsız"
		}
		return strconv.Itoa(v) + unit
	}
	q := func(v string) string {
		if v == "" {
			return "Platform varsayılanı"
		}
		return v
	}
	return [][2]string{
		{"Build zaman aşımı", n(l.BuildTimeout, " dk")},
		{"Süreç başına kopya", n(l.Replicas, "")},
		{"Cron işi", n(l.Crons, "")},
		{"Proje başına CPU kotası", q(l.CPU)},
		{"Proje başına bellek kotası", q(l.Memory)},
	}
}

func (s *Server) usagePage(w http.ResponseWriter, r *http.Request) {
	teams, err := s.Auth.Teams(r.Context(), me(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(teams) == 0 {
		s.render(w, r, http.StatusOK, "usage", "Kullanım", map[string]any{"Teams": teams}, "")
		return
	}
	pick := teams[0]
	if slug := r.URL.Query().Get("team"); slug != "" {
		found := false
		for _, t := range teams {
			if t.Slug == slug {
				pick, found = t, true
			}
		}
		if !found {
			s.errorPage(w, r, http.StatusNotFound, "Ekip bulunamadı.")
			return
		}
	}
	st, err := api.PlansFor(s.Plans, s.Store).Status(r.Context(), pick.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "usage", "Kullanım", map[string]any{
		"Teams": teams, "Team": pick, "Status": st, "Bars": usageBars(st), "Limits": limitRows(st.Limits),
		"PlanLabel": planLabel(st.Plan), "Enforced": s.Plans != nil,
	}, "")
}

// ---- admin ----

func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Auth.PlatformAdmin(me(r)) {
			s.errorPage(w, r, http.StatusForbidden, "Bu sayfa yalnızca platform yöneticileri içindir.")
			return
		}
		h(w, r)
	}
}

// adminFlashes are fixed messages of the admin page, selected by ?ok=.
var adminFlashes = map[string]string{
	"plan":        "Plan kaydedildi.",
	"suspended":   "Ekip askıya alındı; uygulamaları sıfıra ölçeklendi.",
	"unsuspended": "Ekibin askısı kaldırıldı.",
	"user":        "Kullanıcı askıya alındı.",
	"user-off":    "Kullanıcının askısı kaldırıldı.",
}

func (s *Server) adminPage(w http.ResponseWriter, r *http.Request) {
	s.renderAdmin(w, r, http.StatusOK, "")
}

func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	teams, err := api.AdminTeams(r.Context(), s.Store, s.Plans)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	users, err := s.Store.SuspendedUsers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	rows := make([]map[string]any, 0, len(teams))
	for _, t := range teams {
		overrides := ""
		if o := string(t.Overrides); o != "" && o != "{}" {
			overrides = o
		}
		rows = append(rows, map[string]any{"T": t, "Bars": usageBars(t.TeamStatus), "Overrides": overrides,
			"Lines": overrideLines(t.Overrides), "PlanLabel": planLabel(t.Plan)})
	}
	s.render(w, r, status, "admin", "Yönetim", map[string]any{
		"Teams": rows, "Users": users, "Plans": plans.Plans, "PlanLabels": planLabels,
		"Flash": adminFlashes[r.URL.Query().Get("ok")], "Keys": plans.Keys,
	}, errMsg)
}

func (s *Server) adminTeam(w http.ResponseWriter, r *http.Request) (store.Team, bool) {
	t, err := s.Store.GetTeamBySlug(r.Context(), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		s.renderAdmin(w, r, http.StatusNotFound, "Ekip bulunamadı.")
		return t, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return t, false
	}
	return t, true
}

// overrideLines renders stored overrides as the form's key=value lines.
func overrideLines(raw []byte) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v := fmt.Sprint(m[k])
		if f, ok := m[k].(float64); ok && f < 0 {
			v = "unlimited"
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return b.String()
}

// parseOverrides reads "key=value" lines of the overrides field.
func parseOverrides(text string) (map[string]any, error) {
	out := map[string]any{}
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == ',' }) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, errors.New("limits must be key=value lines (e.g. projects=50)")
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

func (s *Server) adminSetPlan(w http.ResponseWriter, r *http.Request) {
	t, ok := s.adminTeam(w, r)
	if !ok {
		return
	}
	limits, err := parseOverrides(r.PostFormValue("limits"))
	if err == nil {
		err = api.SetTeamPlan(r.Context(), s.Store, t, r.PostFormValue("plan"), limits)
	}
	var in *api.InputError
	if errors.As(err, &in) || (err != nil && strings.HasPrefix(err.Error(), "limits must be")) {
		s.renderAdmin(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("web: team plan changed", "team", t.Slug, "plan", r.PostFormValue("plan"), "by", me(r).Login)
	http.Redirect(w, r, "/admin?ok=plan#team-"+t.Slug, http.StatusSeeOther)
}

func (s *Server) adminSuspend(w http.ResponseWriter, r *http.Request) {
	t, ok := s.adminTeam(w, r)
	if !ok {
		return
	}
	_, clusterErr, err := api.SuspendTeam(r.Context(), s.Store, s.Suspender, s.Log, t, r.PostFormValue("reason"), me(r).Login)
	s.adminSuspendResult(w, r, t, clusterErr, err, "suspended")
}

func (s *Server) adminUnsuspend(w http.ResponseWriter, r *http.Request) {
	t, ok := s.adminTeam(w, r)
	if !ok {
		return
	}
	clusterErr, err := api.UnsuspendTeam(r.Context(), s.Store, s.Suspender, s.Log, t, me(r).Login)
	s.adminSuspendResult(w, r, t, clusterErr, err, "unsuspended")
}

func (s *Server) adminSuspendResult(w http.ResponseWriter, r *http.Request, t store.Team, clusterErr, err error, ok string) {
	var in *api.InputError
	switch {
	case errors.As(err, &in):
		s.renderAdmin(w, r, http.StatusBadRequest, in.Msg)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	case clusterErr != nil:
		s.Log.Error("web: suspension: cluster", "team", t.Slug, "err", clusterErr)
		s.renderAdmin(w, r, http.StatusBadGateway,
			"Kaydedildi ama bazı uygulamalar ölçeklenemedi; işlemi tekrarla. Ayrıntılar sunucu logunda.")
		return
	}
	http.Redirect(w, r, "/admin?ok="+ok+"#team-"+t.Slug, http.StatusSeeOther)
}

func (s *Server) adminSuspendUser(w http.ResponseWriter, r *http.Request) {
	u, err := api.SuspendUser(r.Context(), s.Store, s.Auth, r.PostFormValue("login"), r.PostFormValue("reason"))
	s.adminUserResult(w, r, u, err, "user")
}

func (s *Server) adminUnsuspendUser(w http.ResponseWriter, r *http.Request) {
	u, err := api.UnsuspendUser(r.Context(), s.Store, r.PostFormValue("login"))
	s.adminUserResult(w, r, u, err, "user-off")
}

func (s *Server) adminUserResult(w http.ResponseWriter, r *http.Request, u store.User, err error, ok string) {
	var in *api.InputError
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.renderAdmin(w, r, http.StatusNotFound, "Bu GitHub kullanıcı adıyla giriş yapmış bir kullanıcı yok.")
		return
	case errors.As(err, &in):
		s.renderAdmin(w, r, http.StatusConflict, in.Msg)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("web: user suspension changed", "user", u.Login, "ok", ok, "by", me(r).Login)
	http.Redirect(w, r, "/admin?ok="+ok+"#users", http.StatusSeeOther)
}

// ---- legal pages ----

func (s *Server) legalData() map[string]any {
	op := s.Legal.Operator
	if op == "" {
		op = "paas"
	}
	return map[string]any{"Contact": s.Legal.ContactEmail, "Operator": op, "Domain": s.Domain}
}

func (s *Server) termsPage(w http.ResponseWriter, r *http.Request) {
	s.publicPage(w, r, "terms", "Kullanım koşulları")
}

func (s *Server) privacyPage(w http.ResponseWriter, r *http.Request) {
	s.publicPage(w, r, "privacy", "Gizlilik politikası")
}

// publicPage renders a page that needs no sign-in; a signed-in visitor
// still sees the navigation.
func (s *Server) publicPage(w http.ResponseWriter, r *http.Request, name, title string) {
	if p, err := s.Auth.Session(r); err == nil {
		r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	}
	s.render(w, r, http.StatusOK, name, title, s.legalData(), "")
}
