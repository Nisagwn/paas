package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/nisagwn/paas/internal/domains"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// DomainChecker verifies a custom domain right away (domains.Verifier).
type DomainChecker interface {
	Check(ctx context.Context, d store.Domain) (store.Domain, error)
}

// DomainView is a custom domain with the DNS records its owner must create.
type DomainView struct {
	store.Domain
	URL string `json:"url"`
	// Serving: an Ingress routes the hostname to production.
	Serving    bool             `json:"serving"`
	DNSRecords []domains.Record `json:"dns_records"`
}

// NewDomainView is shared with the web UI.
func NewDomainView(d store.Domain, platformDomain, scheme string) DomainView {
	return DomainView{
		Domain: d, URL: naming.URL(scheme, d.Hostname), Serving: d.Routed,
		DNSRecords: domains.Records(d.Hostname, d.AppName, platformDomain, d.Token),
	}
}

// AddDomain validates and stores a new domain, then checks it once so an
// owner who created the DNS records beforehand sees it verified at once.
// Errors are user-facing messages; status is the HTTP status to answer with.
func AddDomain(ctx context.Context, st *store.Store, checker DomainChecker, app store.App, platformDomain, host string) (store.Domain, int, error) {
	h, err := domains.Normalize(host, platformDomain)
	if err != nil {
		return store.Domain{}, http.StatusBadRequest, err
	}
	d, err := st.AddDomain(ctx, app.ID, h, domains.NewToken())
	switch {
	case errors.Is(err, store.ErrConflict):
		return d, http.StatusConflict, errors.New(h + " is already added to an app")
	case errors.Is(err, store.ErrTooManyDomains):
		return d, http.StatusConflict, errors.New("an app can have at most 20 custom domains")
	case err != nil:
		return d, http.StatusInternalServerError, err
	}
	if checker != nil {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if checked, err := checker.Check(ctx, d); err == nil {
			d = checked
		}
	}
	return d, http.StatusCreated, nil
}

type addDomainRequest struct {
	Hostname string `json:"hostname"`
}

func (s *Server) listDomains(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	list, err := s.Store.ListDomains(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := make([]DomainView, 0, len(list))
	for _, d := range list {
		out = append(out, NewDomainView(d, s.Domain, s.Scheme))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) addDomain(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var req addDomainRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, `body must be {"hostname": "www.example.com"}`)
		return
	}
	d, status, err := AddDomain(r.Context(), s.Store, s.Domains, app, s.Domain, req.Hostname)
	if status == http.StatusInternalServerError {
		s.internalError(w, err)
		return
	}
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.Log.Info("domain added", "app", app.Name, "domain", d.Hostname, "status", d.Status)
	writeJSON(w, status, NewDomainView(d, s.Domain, s.Scheme))
}

func (s *Server) lookupDomain(w http.ResponseWriter, r *http.Request) (store.App, store.Domain, bool) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return app, store.Domain{}, false
	}
	d, err := s.Store.GetDomain(r.Context(), app.ID, r.PathValue("hostname"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "domain not found for this app")
		return app, d, false
	}
	if err != nil {
		s.internalError(w, err)
		return app, d, false
	}
	return app, d, true
}

// verifyDomain re-runs the DNS check now instead of waiting for the loop.
func (s *Server) verifyDomain(w http.ResponseWriter, r *http.Request) {
	_, d, ok := s.lookupDomain(w, r)
	if !ok {
		return
	}
	if s.Domains == nil {
		writeError(w, http.StatusNotImplemented, "domain verification is not running")
		return
	}
	d, err := s.Domains.Check(r.Context(), d)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		// The outcome is stored; routing errors are retried by the loop.
		s.Log.Error("domain check", "domain", d.Hostname, "err", err)
	}
	writeJSON(w, http.StatusOK, NewDomainView(d, s.Domain, s.Scheme))
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request) {
	app, d, ok := s.lookupDomain(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteDomain(r.Context(), app.ID, d.Hostname); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, err)
		return
	}
	s.Log.Info("domain deleted", "app", app.Name, "domain", d.Hostname)
	if s.Router != nil && d.Routed {
		if err := s.Router.SyncApp(r.Context(), app.Name); err != nil {
			s.Log.Error("domain delete: route sync", "app", app.Name, "err", err)
			writeError(w, http.StatusBadGateway,
				"domain deleted, but removing its route failed (retried automatically): "+err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
