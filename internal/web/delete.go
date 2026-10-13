package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/appdelete"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 28: "Projeyi sil" in Ayarlar › Tehlikeli bölge. The form posts the
// typed project name; the server compares it (the page's script only
// enables the button once it matches). Team owners and platform admins
// may delete; the button is disabled with the reason for everyone else.

func (s *Server) deleter() *appdelete.Service {
	if s.Deleter != nil {
		return s.Deleter
	}
	return &appdelete.Service{Store: s.Store, Log: s.Log}
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.Store.GetAppByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "Proje bulunamadı.")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	p := me(r)
	if !s.Auth.PlatformAdmin(p) {
		role, err := s.Auth.TeamRole(r.Context(), p, app.TeamID)
		switch {
		case err != nil:
			s.internalError(w, r, err)
			return
		case role == "":
			s.errorPage(w, r, http.StatusNotFound, "Proje bulunamadı.")
			return
		case role != store.RoleOwner:
			s.errorPage(w, r, http.StatusForbidden, msgDeleteOwner)
			return
		}
	}
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))
	res, err := s.deleter().Delete(r.Context(), app, confirm, api.Actor(p))
	switch {
	case errors.Is(err, appdelete.ErrConfirm):
		s.renderTabOf(w, r, app, http.StatusBadRequest, tabSettings, fmt.Sprintf(msgDeleteConfirm, app.Name),
			func(v *appDetail) { v.DeleteInvalid = true })
		return
	case errors.Is(err, store.ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound, "Proje bulunamadı.")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("web: project deleted", "app", app.Name, "by", p.Login, "done", res.Done)
	flash := "deleted"
	if !res.Done {
		flash = "deleting"
	}
	redirect(w, r, "/?ok="+flash+"&app="+url.QueryEscape(app.Name))
}

// deletedFlash is the dashboard's message after a deletion. The name is
// echoed only when it is a valid app name, so nothing else is reflected.
func deletedFlash(r *http.Request) (string, bool) {
	q := r.URL.Query()
	name := q.Get("app")
	if !naming.ValidAppName(name) {
		return "", false
	}
	switch q.Get("ok") {
	case "deleted":
		return fmt.Sprintf(flashDeleted, name), true
	case "deleting":
		return fmt.Sprintf(flashDeleting, name), true
	}
	return "", false
}
