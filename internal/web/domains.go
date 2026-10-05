package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/store"
)

// Custom domains (Faz 12): list with status and DNS instructions, add,
// check now and delete. Forms post with the CSRF token like the env forms;
// htmx swaps the #domains-card fragment.

func (s *Server) domainViews(ctx context.Context, app store.App) ([]api.DomainView, error) {
	list, err := s.Store.ListDomains(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	out := make([]api.DomainView, 0, len(list))
	for _, d := range list {
		out = append(out, api.NewDomainView(d, s.Domain, s.Scheme))
	}
	return out, nil
}

func (s *Server) addDomain(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	d, status, err := api.AddDomain(r.Context(), s.Store, s.Domains, app, s.Domain, r.PostFormValue("hostname"))
	if status == http.StatusInternalServerError {
		s.internalError(w, r, err)
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	} else {
		s.Log.Info("domain added", "app", app.Name, "domain", d.Hostname, "status", d.Status, "via", "web")
		status = http.StatusOK
	}
	s.domainsResponse(w, r, app, status, msg)
}

func (s *Server) verifyDomain(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	status, msg := http.StatusOK, ""
	d, err := s.Store.GetDomain(r.Context(), app.ID, r.PostFormValue("hostname"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		status, msg = http.StatusNotFound, "Alan adı bulunamadı."
	case err != nil:
		s.internalError(w, r, err)
		return
	case s.Domains == nil:
		status, msg = http.StatusNotImplemented, "Alan adı doğrulama şu an çalışmıyor."
	default:
		if _, err := s.Domains.Check(r.Context(), d); err != nil {
			s.Log.Error("domain check", "domain", d.Hostname, "err", err)
		}
	}
	s.domainsResponse(w, r, app, status, msg)
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	status, msg := http.StatusOK, ""
	host := r.PostFormValue("hostname")
	d, err := s.Store.GetDomain(r.Context(), app.ID, host)
	if err == nil {
		err = s.Store.DeleteDomain(r.Context(), app.ID, host)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		status, msg = http.StatusNotFound, "Alan adı bulunamadı."
	case err != nil:
		s.internalError(w, r, err)
		return
	default:
		s.Log.Info("domain deleted", "app", app.Name, "domain", host, "via", "web")
		if s.Router != nil && d.Routed {
			if err := s.Router.SyncApp(r.Context(), app.Name); err != nil {
				s.Log.Error("domain delete: route sync", "app", app.Name, "err", err)
				status, msg = http.StatusBadGateway, "Alan adı silindi ama yönlendirmesi kaldırılamadı; otomatik olarak tekrar denenecek."
			}
		}
	}
	s.domainsResponse(w, r, app, status, msg)
}

// domainsResponse renders the domains fragment for htmx, the app page with
// the error otherwise, or redirects back after success.
func (s *Server) domainsResponse(w http.ResponseWriter, r *http.Request, app store.App, status int, msg string) {
	switch {
	case isHTMX(r):
		v, err := s.appDetail(r.Context(), app)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		var buf bytes.Buffer
		p := page{Data: v, CSRF: s.Sessions.CSRFToken(r), DomainError: tr(msg)}
		if err := s.pages["domains"].Execute(&buf, p); err != nil {
			s.internalError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		buf.WriteTo(w)
	case msg != "":
		s.renderApp(w, r, status, msg)
	default:
		http.Redirect(w, r, "/apps/"+app.Name+"?ok=domain#custom-domains", http.StatusSeeOther)
	}
}
