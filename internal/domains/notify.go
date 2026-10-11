package domains

import (
	"context"
	"time"

	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 26: notifications about custom domains.

// CertificateExpiry reports when a custom domain's certificate expires
// (deploy.Kubernetes reads cert-manager's status.notAfter). ok is false
// when it is not known (no certificate yet, TLS off).
type CertificateExpiry interface {
	DomainCertificateExpiry(ctx context.Context, app, host string) (notAfter time.Time, ok bool, err error)
}

// ExpiryWarning: a certificate expiring within this window has not been
// renewed although cert-manager renews 30 days ahead, so renewal is stuck.
const ExpiryWarning = 14 * 24 * time.Hour

// PendingWarning: a new domain still unverified after this long is reported.
const PendingWarning = 24 * time.Hour

// notifyCheck emits the events of a check whose outcome was saved. Event
// keys make repeated checks (and replicas) report each problem once.
func (v *Verifier) notifyCheck(ctx context.Context, wasStatus string, d store.Domain, now time.Time) {
	if v.Notify == nil {
		return
	}
	var events []notify.Event
	switch {
	case d.Status == store.DomainError && wasStatus != store.DomainError:
		events = append(events, notify.DomainFailure(d, false))
	case d.Status == store.DomainPending && now.Sub(d.CreatedAt) > PendingWarning:
		events = append(events, notify.DomainFailure(d, true))
	case d.Status == store.DomainActive:
		if ce, ok := v.Certs.(CertificateExpiry); ok && v.Certs != nil {
			notAfter, known, err := ce.DomainCertificateExpiry(ctx, d.AppName, d.Hostname)
			if err != nil {
				v.Log.Warn("domain certificate expiry", "domain", d.Hostname, "err", err)
			} else if known && notAfter.Sub(now) < ExpiryWarning {
				events = append(events, notify.CertificateExpiry(d, notAfter, now))
			}
		}
	}
	for _, e := range events {
		if err := v.Notify.Emit(ctx, e); err != nil {
			v.Log.Warn("domain notification", "domain", d.Hostname, "type", e.Type, "err", err)
		}
	}
}
