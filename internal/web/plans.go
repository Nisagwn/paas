package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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
	Key   string // usageAtLimit key
	Label string
	Used  int64
	Max   int // plans.Unlimited: no bound
	Unit  string
	Note  string
	// Reset is when a windowed counter starts again (UTC); zero for totals.
	Reset time.Time
}

// AtLimit says what happens when the bar is full.
func (b usageBar) AtLimit() string { return usageAtLimit[b.Key] }

// LevelLabel names the bar's level for people (and screen readers).
func (b usageBar) LevelLabel() string {
	switch b.Level() {
	case "full":
		return "Limitte"
	case "warn":
		return "Limite yakın"
	}
	return ""
}

// Left is what remains until the limit (never negative).
func (b usageBar) Left() int64 {
	if b.Max < 0 || int64(b.Max) <= b.Used {
		return 0
	}
	return int64(b.Max) - b.Used
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
		{Key: "projects", Label: "Projeler", Used: int64(u.Projects), Max: l.Projects},
		{Key: "build_minutes", Label: "Build dakikası (bu ay)", Used: st.BuildMinutes, Max: l.BuildMinutes, Unit: "dk",
			Note: "Her ayın ilk günü (UTC) sıfırlanır.", Reset: st.MonthStart.AddDate(0, 1, 0)},
		{Key: "deploys", Label: "Deploy (bugün)", Used: int64(u.DeploymentsToday), Max: l.DeploymentsPerDay,
			Note: "Her gün 00:00 UTC'de sıfırlanır.", Reset: st.DayStart.AddDate(0, 0, 1)},
		{Key: "addons", Label: "Veritabanları", Used: int64(u.Addons), Max: l.Addons},
		{Key: "domains", Label: "Alan adları", Used: int64(u.Domains), Max: l.Domains},
		{Key: "builds", Label: "Eşzamanlı build", Used: int64(u.ActiveBuilds), Max: l.ConcurrentBuilds,
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
	bars := usageBars(st)
	worst := "ok"
	for _, b := range bars {
		switch b.Level() {
		case "full":
			worst = "full"
		case "warn":
			if worst == "ok" {
				worst = "warn"
			}
		}
	}
	s.render(w, r, http.StatusOK, "usage", "Kullanım", map[string]any{
		"Teams": teams, "Team": pick, "Status": st, "Bars": bars, "Limits": limitRows(st.Limits),
		"PlanLabel": planLabel(st.Plan), "Enforced": s.Plans != nil, "Worst": worst,
		"Contact": s.Legal.ContactEmail, "Admin": s.Auth.PlatformAdmin(me(r)),
		"Owner": pick.Role == store.RoleOwner,
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
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	filter := r.URL.Query().Get("filter") // "", "suspended" or "limit"
	rows := make([]adminRow, 0, len(teams))
	var suspended, atLimit int
	for _, t := range teams {
		row := adminRow{T: t, Bars: usageBars(t.TeamStatus), Lines: overrideLines(t.Overrides), PlanLabel: planLabel(t.Plan)}
		if o := string(t.Overrides); o != "" && o != "{}" {
			row.Overrides = o
		}
		for _, b := range row.Bars {
			switch b.Level() {
			case "full":
				row.Level = "full"
			case "warn":
				if row.Level != "full" {
					row.Level = "warn"
				}
			}
		}
		if t.Suspended() {
			suspended++
		}
		if row.Level == "full" {
			atLimit++
		}
		if q != "" && !strings.Contains(strings.ToLower(t.Slug+" "+t.Name+" "+strings.Join(t.Owners, " ")), q) {
			continue
		}
		if (filter == "suspended" && !t.Suspended()) || (filter == "limit" && row.Level != "full") {
			continue
		}
		rows = append(rows, row)
	}
	s.render(w, r, status, "admin", "Yönetim", map[string]any{
		"Teams": rows, "Total": len(teams), "Suspended": suspended, "AtLimit": atLimit,
		"Q": r.URL.Query().Get("q"), "Filter": filter,
		"Users": users, "Plans": plans.Plans, "PlanLabels": planLabels,
		"Flash": adminFlashes[r.URL.Query().Get("ok")], "Keys": plans.Keys,
	}, errMsg)
}

// adminRow is one team of the admin list.
type adminRow struct {
	T         api.AdminTeamView
	Bars      []usageBar
	Overrides string
	Lines     string
	PlanLabel string
	// Level is the worst bar: "", "warn" or "full".
	Level string
}

// Bar returns the bar with key (zero bar when missing).
func (a adminRow) Bar(key string) usageBar {
	for _, b := range a.Bars {
		if b.Key == key {
			return b
		}
	}
	return usageBar{}
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
	// The inline plan form has no limits field: it keeps the overrides.
	var limits map[string]any
	var err error
	if r.ParseForm(); r.PostForm.Has("limits") {
		limits, err = parseOverrides(r.PostFormValue("limits"))
	}
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
	if strings.TrimSpace(r.PostFormValue("reason")) == "" {
		s.renderAdmin(w, r, http.StatusBadRequest, "Askıya alma gerekçesi yaz; ekibin üyeleri bu metni görür.")
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
