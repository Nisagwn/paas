package main

import (
	"log/slog"

	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/config"
	"github.com/nisagwn/paas/internal/protect"
	"github.com/nisagwn/paas/internal/store"
)

// newProtection is deployment protection (Faz 24): the ForwardAuth
// endpoint (/_auth/verify, mounted by the API) and the login page's tokens
// (/_protect/login, the web UI). Cookies and tokens are signed with the
// session key (PAAS_SESSION_KEY). The deployer attaches the Middleware with
// PAAS_FORWARD_AUTH_URL (newPipeline).
func newProtection(cfg config.Config, st *store.Store, sessions *auth.Sessions, authn *auth.Authenticator, log *slog.Logger) *protect.Guard {
	return &protect.Guard{
		Store: st, Sessions: sessions, Auth: authn, Domain: cfg.Domain, PublicURL: cfg.PublicURL, Log: log,
	}
}
