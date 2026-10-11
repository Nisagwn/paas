package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 26: notification channels of a team and the caller's email
// preferences.
//
//	GET    /api/teams/{slug}/notifications                 viewer
//	POST   /api/teams/{slug}/notifications                 member (webhook: signing_secret shown once)
//	GET    /api/teams/{slug}/notifications/{id}            viewer
//	PUT    /api/teams/{slug}/notifications/{id}            member
//	DELETE /api/teams/{slug}/notifications/{id}            member
//	POST   /api/teams/{slug}/notifications/{id}/test       member
//	GET    /api/teams/{slug}/notifications/{id}/deliveries viewer
//	GET    /api/notifications/events                       any caller
//	GET    /api/me/notifications                           any caller
//	PUT    /api/me/notifications                           a signed-in user
//
// Channel URLs and signing secrets are never returned after creation.

func (s *Server) notificationRoutes(api *http.ServeMux) {
	viewer, member := store.RoleViewer, store.RoleMember
	api.HandleFunc("GET /api/teams/{slug}/notifications", s.requireTeam(viewer, s.listChannels))
	api.HandleFunc("POST /api/teams/{slug}/notifications", s.requireTeam(member, s.createChannel))
	api.HandleFunc("GET /api/teams/{slug}/notifications/{id}", s.requireTeam(viewer, s.getChannel))
	api.HandleFunc("PUT /api/teams/{slug}/notifications/{id}", s.requireTeam(member, s.updateChannel))
	api.HandleFunc("DELETE /api/teams/{slug}/notifications/{id}", s.requireTeam(member, s.deleteChannel))
	api.HandleFunc("POST /api/teams/{slug}/notifications/{id}/test", s.requireTeam(member, s.testChannel))
	api.HandleFunc("GET /api/teams/{slug}/notifications/{id}/deliveries", s.requireTeam(viewer, s.channelDeliveries))
	api.HandleFunc("GET /api/notifications/events", s.notificationEvents)
	api.HandleFunc("GET /api/me/notifications", s.getNotificationPrefs)
	api.HandleFunc("PUT /api/me/notifications", s.putNotificationPrefs)
}

// notifications returns the configured service, or one that stores
// channels and queues events without sending (no dispatcher).
func (s *Server) notifications() *notify.Service {
	if s.Notifications != nil {
		return s.Notifications
	}
	return &notify.Service{Store: s.Store, Log: s.Log}
}

// productionChanged emits production.changed after a user moved the
// production alias. Best effort.
func (s *Server) productionChanged(r *http.Request, deploymentID int64, reason string) {
	if s.Notifications == nil {
		return
	}
	notify.EmitProductionChange(r.Context(), s.Notifications, s.Store, s.Log, deploymentID, reason, principal(r).Login)
}

// requireTeam resolves {slug}, checks the caller's role and hands the team
// to h (404 when the caller is not in the team, 403 below need).
func (s *Server) requireTeam(need string, h func(http.ResponseWriter, *http.Request, store.Team)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, err := s.Auth.Team(r.Context(), principal(r), r.PathValue("slug"))
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "team not found")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		if !store.RoleAllows(t.Role, need) {
			writeError(w, http.StatusForbidden, "this action needs the "+need+" role on the team")
			return
		}
		h(w, r, t)
	}
}

func (s *Server) teamChannel(w http.ResponseWriter, r *http.Request, t store.Team) (store.NotificationChannel, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "notification channel not found")
		return store.NotificationChannel{}, false
	}
	ch, err := s.Store.GetNotificationChannel(r.Context(), t.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "notification channel not found")
		return ch, false
	}
	if err != nil {
		s.internalError(w, err)
		return ch, false
	}
	return ch, true
}

func (s *Server) channelError(w http.ResponseWriter, err error) {
	if msg, ok := notify.IsInvalid(err); ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	s.internalError(w, err)
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request, t store.Team) {
	chs, err := s.Store.ListNotificationChannels(r.Context(), t.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chs)
}

// channelView is a created or updated channel; SigningSecret is only set
// when a new one was issued, and is never shown again.
type channelView struct {
	store.NotificationChannel
	SigningSecret string `json:"signing_secret,omitempty"`
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request, t store.Team) {
	var req notify.ChannelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ch, secret, err := s.notifications().CreateChannel(r.Context(), t.ID, req, principal(r).Login)
	if err != nil {
		s.channelError(w, err)
		return
	}
	s.Log.Info("notification channel created", "team", t.Slug, "channel", ch.ID, "kind", ch.Kind, "by", principal(r).Login)
	writeJSON(w, http.StatusCreated, channelView{NotificationChannel: ch, SigningSecret: secret})
}

func (s *Server) getChannel(w http.ResponseWriter, r *http.Request, t store.Team) {
	if ch, ok := s.teamChannel(w, r, t); ok {
		writeJSON(w, http.StatusOK, ch)
	}
}

func (s *Server) updateChannel(w http.ResponseWriter, r *http.Request, t store.Team) {
	ch, ok := s.teamChannel(w, r, t)
	if !ok {
		return
	}
	var req notify.ChannelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, secret, err := s.notifications().UpdateChannel(r.Context(), ch, req)
	if err != nil {
		s.channelError(w, err)
		return
	}
	s.Log.Info("notification channel updated", "team", t.Slug, "channel", ch.ID, "by", principal(r).Login)
	writeJSON(w, http.StatusOK, channelView{NotificationChannel: out, SigningSecret: secret})
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request, t store.Team) {
	ch, ok := s.teamChannel(w, r, t)
	if !ok {
		return
	}
	if err := s.Store.DeleteNotificationChannel(r.Context(), t.ID, ch.ID); err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("notification channel deleted", "team", t.Slug, "channel", ch.ID, "by", principal(r).Login)
	w.WriteHeader(http.StatusNoContent)
}

// testChannel sends a test message now: 200 sent, 502 failed (the
// delivery says why), 202 queued (a retry is due, or no dispatcher runs).
func (s *Server) testChannel(w http.ResponseWriter, r *http.Request, t store.Team) {
	ch, ok := s.teamChannel(w, r, t)
	if !ok {
		return
	}
	d, err := s.notifications().SendTest(r.Context(), ch, principal(r).Login)
	if err != nil {
		s.internalError(w, err)
		return
	}
	status := http.StatusAccepted
	switch d.Status {
	case store.DeliverySent:
		status = http.StatusOK
	case store.DeliveryFailed, store.DeliverySkipped:
		status = http.StatusBadGateway
	}
	writeJSON(w, status, d)
}

func (s *Server) channelDeliveries(w http.ResponseWriter, r *http.Request, t store.Team) {
	ch, ok := s.teamChannel(w, r, t)
	if !ok {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	ds, err := s.Store.ListNotificationDeliveries(r.Context(), ch.ID, limit)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ds)
}

type eventTypeView struct {
	Type    string `json:"type"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
}

func (s *Server) notificationEvents(w http.ResponseWriter, r *http.Request) {
	def := map[string]bool{}
	for _, e := range notify.DefaultEvents {
		def[e] = true
	}
	out := make([]eventTypeView, 0, len(notify.EventTypes))
	for _, e := range notify.EventTypes {
		out = append(out, eventTypeView{Type: e, Label: notify.EventLabels[e], Default: def[e]})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getNotificationPrefs(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetNotificationPrefs(r.Context(), principal(r).UserID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type prefsRequest struct {
	Email        string   `json:"email"`
	EmailEnabled *bool    `json:"email_enabled"`
	MutedEvents  []string `json:"muted_events"`
}

func (s *Server) putNotificationPrefs(w http.ResponseWriter, r *http.Request) {
	uid := principal(r).UserID
	if uid == 0 {
		writeError(w, http.StatusBadRequest, "email preferences belong to a user; sign in with GitHub")
		return
	}
	var req prefsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p := store.NotificationPrefs{UserID: uid, Email: req.Email, EmailEnabled: true, MutedEvents: req.MutedEvents}
	if req.EmailEnabled != nil {
		p.EmailEnabled = *req.EmailEnabled
	}
	if err := notify.CheckPrefs(&p); err != nil {
		s.channelError(w, err)
		return
	}
	out, err := s.Store.SetNotificationPrefs(r.Context(), p)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
