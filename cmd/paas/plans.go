package main

import (
	"log/slog"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/config"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/routing"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/web"
	"github.com/nisagwn/paas/internal/worker"
)

// setupPlans wires Faz 23 (plans, rate limits, suspension, legal pages)
// into the servers, the worker and the deployer.
func setupPlans(pub config.Public, cfg config.Config, st *store.Store, applier routing.Applier,
	authn *auth.Authenticator, gl *auth.GitHubLogin, apiSrv *api.Server, ui *web.Server, w *worker.Worker, log *slog.Logger) {
	guard := &plans.Guard{Store: st, Catalog: pub.Plans}
	limits := pub.RateLimits()
	authn.Admins = cfg.AdminGitHubLogins
	if gl != nil {
		gl.MinAccountAge = pub.MinAccountAge
	}
	apiSrv.Plans, apiSrv.Limits = guard, limits
	ui.Plans, ui.Limits = guard, limits
	ui.Legal = web.Legal{ContactEmail: pub.ContactEmail, Operator: pub.Operator}
	w.Plans = guard
	if k, ok := applier.(*deploy.Kubernetes); ok {
		k.Plans = guard
		apiSrv.Suspender, ui.Suspender = k, k
	}
	if r, ok := w.Notifier.(api.RefusalReporter); ok {
		apiSrv.Refusals = r
	}
	log.Info("plans and rate limits enabled",
		"hobby", pub.Plans[store.PlanHobby], "pro", pub.Plans[store.PlanPro],
		"rate_signin", limits.SignIn.Rule().String(), "rate_import", limits.Import.Rule().String(),
		"rate_writes", limits.Writes.Rule().String(), "trust_forwarded", limits.TrustForwarded,
		"min_account_age", pub.MinAccountAge.String())
}
