package web

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 26: the team's "Bildirimler" page — channels, their delivery log,
// "Test gönder" and the signed-in user's email preferences.

func init() {
	funcs["notifykind"] = func(k string) string {
		if l, ok := notify.KindLabels[k]; ok {
			return l
		}
		return k
	}
	funcs["deliveryclass"] = deliveryClass
	funcs["eventlabel"] = func(t string) string {
		if l, ok := notify.EventLabels[t]; ok {
			return l
		}
		return t
	}
	for k, v := range map[string]string{
		"notify-created":     "Bildirim kanalı eklendi.",
		"notify-saved":       "Bildirim kanalı kaydedildi.",
		"notify-deleted":     "Bildirim kanalı silindi.",
		"notify-test-sent":   "Test mesajı gönderildi.",
		"notify-test-queued": "Test mesajı kuyruğa alındı; birkaç saniye içinde gönderilir.",
		"notify-prefs":       "E-posta tercihlerin kaydedildi.",
	} {
		flashes[k] = v
	}
}

func (s *Server) notificationRoutes(mux *http.ServeMux) {
	s.pages["notifications"] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/layout.html", "templates/partials.html", "templates/processes.html", "templates/rollout.html",
		"templates/notifications.html"))
	mux.Handle("GET /teams/{slug}/notifications", s.authed(s.notificationsPage))
	mux.Handle("POST /teams/{slug}/notifications", s.authed(s.createChannel))
	mux.Handle("POST /teams/{slug}/notifications/preferences", s.authed(s.savePrefs))
	mux.Handle("POST /teams/{slug}/notifications/{id}", s.authed(s.updateChannel))
	mux.Handle("POST /teams/{slug}/notifications/{id}/delete", s.authed(s.deleteChannel))
	mux.Handle("POST /teams/{slug}/notifications/{id}/test", s.authed(s.testChannel))
}

func (s *Server) notifications() *notify.Service {
	if s.Notifications != nil {
		return s.Notifications
	}
	return &notify.Service{Store: s.Store, Log: s.Log}
}

// productionChanged emits production.changed after a user moved the
// production alias from the UI. Best effort.
func (s *Server) productionChanged(r *http.Request, deploymentID int64, reason string) {
	if s.Notifications == nil {
		return
	}
	notify.EmitProductionChange(r.Context(), s.Notifications, s.Store, s.Log, deploymentID, reason, me(r).Login)
}

// notifyTeam loads {slug} and checks the caller's role (404 outside the team).
func (s *Server) notifyTeam(w http.ResponseWriter, r *http.Request, need string) (store.Team, bool) {
	t, err := s.Auth.Team(r.Context(), me(r), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Ekip bulunamadı.")
		return t, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return t, false
	}
	if !store.RoleAllows(t.Role, need) {
		s.errorPage(w, r, http.StatusForbidden, "Bu işlem için ekipte "+roleLabel(need)+" rolü gerekir.")
		return t, false
	}
	return t, true
}

type deliveryRow struct {
	store.NotificationDelivery
	Title string
}

type channelRow struct {
	store.NotificationChannel
	Deliveries []deliveryRow
	Subscribed map[string]bool
	InApps     map[string]bool
	Secret     bool // kind has a URL
	// EventGroups are the channel's event checkboxes.
	EventGroups []eventChoiceGroup
	// Icon is the kind's symbol on the card.
	Icon string
}

// Last is the most recent delivery, nil when there is none.
func (c channelRow) Last() *deliveryRow {
	if len(c.Deliveries) == 0 {
		return nil
	}
	return &c.Deliveries[0]
}

// deliveryClass maps a delivery status to a badge class.
func deliveryClass(status string) string {
	switch status {
	case store.DeliverySent:
		return "ready"
	case store.DeliveryFailed:
		return "failed"
	case store.DeliverySending:
		return "building"
	case store.DeliverySkipped:
		return "canceled"
	}
	return "queued"
}

func kindIcon(kind string) string {
	for _, k := range channelKinds {
		if k.Kind == kind {
			return k.Icon
		}
	}
	return "•"
}

type eventOption struct {
	Type, Label string
	Default     bool
}

// notifyForm is the create form as submitted (re-rendered on errors).
type notifyForm struct {
	Name, Kind, URL, Recipients string
	TeamMembers                 bool
	Events, Apps                map[string]bool
	Open                        bool
}

func (s *Server) notificationsPage(w http.ResponseWriter, r *http.Request) {
	t, ok := s.notifyTeam(w, r, store.RoleViewer)
	if !ok {
		return
	}
	s.renderNotifications(w, r, t, http.StatusOK, "", nil, "", "")
}

func (s *Server) renderNotifications(w http.ResponseWriter, r *http.Request, t store.Team, status int, errMsg string,
	form *notifyForm, createdName, secret string) {
	ctx := r.Context()
	chs, err := s.Store.ListNotificationChannels(ctx, t.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	rows := make([]channelRow, 0, len(chs))
	for _, c := range chs {
		ds, err := s.Store.ListNotificationDeliveries(ctx, c.ID, 10)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		row := channelRow{NotificationChannel: c, Subscribed: map[string]bool{}, InApps: map[string]bool{},
			Secret: c.Kind != store.ChannelEmail, Icon: kindIcon(c.Kind)}
		for _, e := range c.Events {
			row.Subscribed[e] = true
		}
		row.EventGroups = eventChoices(func(e string) bool { return row.Subscribed[e] })
		for _, a := range c.Apps {
			row.InApps[a] = true
		}
		for _, d := range ds {
			var ev struct {
				Title string `json:"title"`
			}
			json.Unmarshal(d.Payload, &ev)
			row.Deliveries = append(row.Deliveries, deliveryRow{NotificationDelivery: d, Title: ev.Title})
		}
		rows = append(rows, row)
	}
	all, err := s.Auth.Apps(ctx, me(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var apps []string
	for _, a := range all {
		if a.TeamID == t.ID {
			apps = append(apps, a.Name)
		}
	}
	def := map[string]bool{}
	for _, e := range notify.DefaultEvents {
		def[e] = true
	}
	var events []eventOption
	for _, e := range notify.EventTypes {
		events = append(events, eventOption{Type: e, Label: notify.EventLabels[e], Default: def[e]})
	}
	if form == nil {
		form = &notifyForm{Kind: store.ChannelSlack, Events: def, Apps: map[string]bool{}}
	}
	prefs, err := s.Store.GetNotificationPrefs(ctx, me(r).UserID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	muted := map[string]bool{}
	for _, e := range prefs.MutedEvents {
		muted[e] = true
	}
	formEvents := form.Events
	s.render(w, r, status, "notifications", t.Name+" · Bildirimler", map[string]any{
		"Team": t, "CanEdit": store.RoleAllows(t.Role, store.RoleMember), "Channels": rows, "Events": events,
		"Apps": apps, "SMTP": s.notifications().SMTPConfigured, "Form": form,
		"Kinds":      []string{store.ChannelSlack, store.ChannelDiscord, store.ChannelWebhook, store.ChannelEmail},
		"KindCards":  channelKinds,
		"FormEvents": eventChoices(func(e string) bool { return formEvents[e] }),
		"PrefEvents": eventChoices(func(e string) bool { return !muted[e] }),
		"Created":    createdName, "Secret": secret,
		"Prefs": prefs, "Muted": muted, "HasUser": me(r).UserID != 0,
	}, errMsg)
}

// splitAddresses accepts addresses separated by commas, semicolons or lines.
func splitAddresses(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' || r == ' ' })
}

func formSet(r *http.Request, key string) map[string]bool {
	m := map[string]bool{}
	for _, v := range r.PostForm[key] {
		m[v] = true
	}
	return m
}

// channelRequest reads the create / update form.
func channelRequest(r *http.Request, update bool) notify.ChannelRequest {
	r.ParseForm()
	events := r.PostForm["events"]
	apps := r.PostForm["apps"]
	if events == nil {
		events = []string{}
	}
	if apps == nil {
		apps = []string{}
	}
	recipients := splitAddresses(r.PostFormValue("recipients"))
	members := r.PostFormValue("team_members") != ""
	req := notify.ChannelRequest{
		Name: r.PostFormValue("name"), URL: strings.TrimSpace(r.PostFormValue("url")),
		Events: &events, Apps: &apps,
	}
	if !update {
		req.Kind = r.PostFormValue("kind")
	} else {
		enabled := r.PostFormValue("enabled") != ""
		req.Enabled = &enabled
		req.RotateSecret = r.PostFormValue("rotate_secret") != ""
	}
	if req.Kind == store.ChannelEmail || r.PostFormValue("is_email") != "" {
		req.Recipients, req.TeamMembers = &recipients, &members
	}
	return req
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	t, ok := s.notifyTeam(w, r, store.RoleMember)
	if !ok {
		return
	}
	req := channelRequest(r, false)
	ch, secret, err := s.notifications().CreateChannel(r.Context(), t.ID, req, me(r).Login)
	if err != nil {
		msg, ok := notify.IsInvalid(err)
		if !ok {
			s.internalError(w, r, err)
			return
		}
		form := &notifyForm{Name: req.Name, Kind: req.Kind, URL: req.URL, Recipients: r.PostFormValue("recipients"),
			TeamMembers: r.PostFormValue("team_members") != "", Events: formSet(r, "events"), Apps: formSet(r, "apps"), Open: true}
		s.renderNotifications(w, r, t, http.StatusBadRequest, msg, form, "", "")
		return
	}
	s.Log.Info("web: notification channel created", "team", t.Slug, "channel", ch.ID, "kind", ch.Kind, "by", me(r).Login)
	if secret != "" {
		// Shown once, like a new API token: no redirect.
		s.renderNotifications(w, r, t, http.StatusCreated, "", nil, ch.Name, secret)
		return
	}
	http.Redirect(w, r, "/teams/"+t.Slug+"/notifications?ok=notify-created", http.StatusSeeOther)
}

func (s *Server) formChannel(w http.ResponseWriter, r *http.Request, t store.Team) (store.NotificationChannel, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.errorPage(w, r, http.StatusNotFound, "Bildirim kanalı bulunamadı.")
		return store.NotificationChannel{}, false
	}
	ch, err := s.Store.GetNotificationChannel(r.Context(), t.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Bildirim kanalı bulunamadı.")
		return ch, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return ch, false
	}
	return ch, true
}

func (s *Server) updateChannel(w http.ResponseWriter, r *http.Request) {
	t, ok := s.notifyTeam(w, r, store.RoleMember)
	if !ok {
		return
	}
	ch, ok := s.formChannel(w, r, t)
	if !ok {
		return
	}
	out, secret, err := s.notifications().UpdateChannel(r.Context(), ch, channelRequest(r, true))
	if err != nil {
		msg, ok := notify.IsInvalid(err)
		if !ok {
			s.internalError(w, r, err)
			return
		}
		s.renderNotifications(w, r, t, http.StatusBadRequest, msg, nil, "", "")
		return
	}
	s.Log.Info("web: notification channel updated", "team", t.Slug, "channel", ch.ID, "by", me(r).Login)
	if secret != "" {
		s.renderNotifications(w, r, t, http.StatusOK, "", nil, out.Name, secret)
		return
	}
	http.Redirect(w, r, "/teams/"+t.Slug+"/notifications?ok=notify-saved", http.StatusSeeOther)
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	t, ok := s.notifyTeam(w, r, store.RoleMember)
	if !ok {
		return
	}
	ch, ok := s.formChannel(w, r, t)
	if !ok {
		return
	}
	if err := s.Store.DeleteNotificationChannel(r.Context(), t.ID, ch.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("web: notification channel deleted", "team", t.Slug, "channel", ch.ID, "by", me(r).Login)
	http.Redirect(w, r, "/teams/"+t.Slug+"/notifications?ok=notify-deleted", http.StatusSeeOther)
}

func (s *Server) testChannel(w http.ResponseWriter, r *http.Request) {
	t, ok := s.notifyTeam(w, r, store.RoleMember)
	if !ok {
		return
	}
	ch, ok := s.formChannel(w, r, t)
	if !ok {
		return
	}
	d, err := s.notifications().SendTest(r.Context(), ch, me(r).Login)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	switch d.Status {
	case store.DeliverySent:
		http.Redirect(w, r, "/teams/"+t.Slug+"/notifications?ok=notify-test-sent#channel-"+strconv.FormatInt(ch.ID, 10), http.StatusSeeOther)
	case store.DeliveryFailed, store.DeliverySkipped:
		s.renderNotifications(w, r, t, http.StatusBadGateway, "Test mesajı gönderilemedi: "+d.LastError, nil, "", "")
	default:
		http.Redirect(w, r, "/teams/"+t.Slug+"/notifications?ok=notify-test-queued#channel-"+strconv.FormatInt(ch.ID, 10), http.StatusSeeOther)
	}
}

func (s *Server) savePrefs(w http.ResponseWriter, r *http.Request) {
	t, ok := s.notifyTeam(w, r, store.RoleViewer)
	if !ok {
		return
	}
	if me(r).UserID == 0 {
		s.renderNotifications(w, r, t, http.StatusBadRequest, "E-posta tercihleri bir kullanıcıya aittir; GitHub ile giriş yap.", nil, "", "")
		return
	}
	r.ParseForm()
	// The form lists the events the user wants; the rest are muted.
	want := formSet(r, "events")
	var muted []string
	for _, e := range notify.EventTypes {
		if !want[e] {
			muted = append(muted, e)
		}
	}
	p := store.NotificationPrefs{UserID: me(r).UserID, Email: r.PostFormValue("email"),
		EmailEnabled: r.PostFormValue("email_enabled") != "", MutedEvents: muted}
	if err := notify.CheckPrefs(&p); err != nil {
		msg, _ := notify.IsInvalid(err)
		s.renderNotifications(w, r, t, http.StatusBadRequest, msg, nil, "", "")
		return
	}
	if _, err := s.Store.SetNotificationPrefs(r.Context(), p); err != nil {
		s.internalError(w, r, err)
		return
	}
	http.Redirect(w, r, "/teams/"+t.Slug+"/notifications?ok=notify-prefs#prefs", http.StatusSeeOther)
}
