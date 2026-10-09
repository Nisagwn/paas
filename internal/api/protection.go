package api

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/protect"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 24: deployment protection (internal/protect).
//
//	GET    /api/apps/{name}/protection                    viewer
//	PUT    /api/apps/{name}/protection                    member  {"mode", "scope", "password"}
//	POST   /api/apps/{name}/protection/share-links        member  {"name", "expires_in_hours", "url"}
//	DELETE /api/apps/{name}/protection/share-links/{id}   member
//	POST   /api/apps/{name}/protection/bypass             member  new bypass secret, shown once
//	DELETE /api/apps/{name}/protection/bypass             member
//
// Share tokens and the bypass secret are stored as SHA-256 hashes only;
// listings show their prefix.

type bypassView struct {
	Prefix    string     `json:"prefix"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

type protectionView struct {
	store.Protection
	PasswordSet bool              `json:"password_set"`
	Bypass      *bypassView       `json:"bypass"`
	ShareLinks  []store.ShareLink `json:"share_links"`
	// BypassHeader is the request header for the bypass secret.
	BypassHeader string `json:"bypass_header"`
}

func (s *Server) protectionView(w http.ResponseWriter, r *http.Request, app store.App, status int) {
	p, err := s.Store.GetProtection(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	links, err := s.Store.ListShareLinks(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	v := protectionView{Protection: p, PasswordSet: p.PasswordHash != "", ShareLinks: links, BypassHeader: protect.BypassHeader}
	if p.BypassHash != "" {
		v.Bypass = &bypassView{Prefix: p.BypassPrefix, CreatedAt: p.BypassCreatedAt}
	}
	writeJSON(w, status, v)
}

func (s *Server) getProtection(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	s.protectionView(w, r, app, http.StatusOK)
}

type protectionRequest struct {
	// Mode: off, team or password; empty keeps the current mode.
	Mode string `json:"mode"`
	// Scope: previews or all; empty keeps the current scope.
	Scope string `json:"scope"`
	// Password replaces the stored password when set.
	Password string `json:"password"`
}

func (s *Server) putProtection(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var req protectionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := protect.Save(r.Context(), s.Store, app, req.Mode, req.Scope, req.Password)
	if msg, bad := auth.IsInvalid(err); bad {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("protection updated", "app", app.Name, "mode", p.Mode, "scope", p.Scope,
		"password_changed", req.Password != "", "by", principal(r).Login)
	// The setting is saved; the reconcile loop retries a failed sync.
	if s.Router != nil {
		if err := s.Router.SyncApp(r.Context(), app.Name); err != nil {
			s.Log.Error("protection: route sync", "app", app.Name, "err", err)
			writeError(w, http.StatusBadGateway,
				"protection saved, but updating the router failed (retried automatically): "+err.Error())
			return
		}
	}
	s.protectionView(w, r, app, http.StatusOK)
}

type shareLinkRequest struct {
	Name           string `json:"name"`
	ExpiresInHours int    `json:"expires_in_hours"`
	// URL is the address to share (an address of the app); empty is the
	// production URL.
	URL string `json:"url"`
}

// createdShareView carries the token once; it is never shown again.
type createdShareView struct {
	store.ShareLink
	Token string `json:"token"`
	URL   string `json:"url"`
}

func (s *Server) createShareLink(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var req shareLinkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var target *url.URL
	var err error
	if req.URL == "" {
		target, err = url.Parse(naming.URL(s.Scheme, naming.ProductionHost(app.Name, s.Domain)))
	} else {
		target, err = protect.AppURL(r.Context(), s.Store, app, s.Domain, req.URL)
	}
	if msg, bad := auth.IsInvalid(err); bad {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	l, plain, err := protect.CreateShare(r.Context(), s.Store, app, req.Name, req.ExpiresInHours, principal(r).UserID)
	if msg, bad := auth.IsInvalid(err); bad {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("share link created", "app", app.Name, "link", l.ID, "by", principal(r).Login)
	writeJSON(w, http.StatusCreated, createdShareView{ShareLink: l, Token: plain, URL: protect.ShareURL(target, plain)})
}

func (s *Server) deleteShareLink(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.Store.DeleteShareLink(r.Context(), app.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "share link not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("share link revoked", "app", app.Name, "link", id, "by", principal(r).Login)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createBypass(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	secret, err := protect.RotateBypass(r.Context(), s.Store, app)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("protection bypass rotated", "app", app.Name, "by", principal(r).Login)
	writeJSON(w, http.StatusCreated, map[string]string{"secret": secret, "header": protect.BypassHeader})
}

func (s *Server) deleteBypass(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	if err := protect.RemoveBypass(r.Context(), s.Store, app); err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("protection bypass removed", "app", app.Name, "by", principal(r).Login)
	w.WriteHeader(http.StatusNoContent)
}
