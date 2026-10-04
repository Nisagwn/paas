package domains

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

// Resolver is the part of *net.Resolver the verifier uses.
type Resolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// Router applies an app's routes, custom domains included (routing.Syncer).
type Router interface {
	SyncApp(ctx context.Context, app string) error
}

// Certificates reports whether a routed domain's TLS certificate is issued
// (deploy.Kubernetes). ready is true when TLS is off.
type Certificates interface {
	DomainCertificate(ctx context.Context, app, host string) (ready bool, msg string, err error)
}

// Verification modes.
const (
	ModeDNS  = "dns"
	ModeSkip = "skip" // development only: every domain passes
)

// Verifier checks custom domains and moves them through the states
//
//	pending → verified (DNS ok, routed) → active (certificate ready)
//	verified/active → error (re-validation failed; still routed for Grace)
//	error → not routed once Grace has passed; back to verified when DNS is fixed
//
// Pending, verified and error domains are checked every Interval, active
// ones every Recheck. Pending domains older than a day slow down to Recheck.
type Verifier struct {
	Store    *store.Store
	Resolver Resolver
	// Router is nil without a real deployer: verified domains then count
	// as active immediately.
	Router Router
	// Certs is optional; nil treats certificates as ready.
	Certs Certificates
	// Domain is the platform domain; CNAMEs must point at <app>.<Domain>
	// or <Domain>.
	Domain   string
	Mode     string
	Interval time.Duration // default 1m
	Recheck  time.Duration // default 1h
	Grace    time.Duration // default 72h
	Log      *slog.Logger
	// Now is replaced in tests.
	Now func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Run checks due domains at start and every Interval until ctx ends.
func (v *Verifier) Run(ctx context.Context) {
	if v.Mode == ModeSkip {
		v.Log.Warn("PAAS_DOMAIN_VERIFY=skip: custom domains are routed without DNS verification (development only)")
	}
	t := time.NewTicker(orDefault(v.Interval, time.Minute))
	defer t.Stop()
	for {
		v.CheckDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CheckDue checks every domain that is due and returns how many failed to
// be processed (DNS failures are results, not errors).
func (v *Verifier) CheckDue(ctx context.Context) int {
	all, err := v.Store.ListDomains(ctx, 0)
	if err != nil {
		v.Log.Error("domain check: list", "err", err)
		return 1
	}
	failed := 0
	for _, d := range all {
		if !v.due(d) {
			continue
		}
		if _, err := v.Check(ctx, d); err != nil && !errors.Is(err, store.ErrNotFound) {
			failed++
			v.Log.Error("domain check", "app", d.AppName, "domain", d.Hostname, "err", err)
		}
	}
	return failed
}

func (v *Verifier) due(d store.Domain) bool {
	if d.LastCheckedAt == nil {
		return true
	}
	since := v.now().Sub(*d.LastCheckedAt)
	recheck := orDefault(v.Recheck, time.Hour)
	switch {
	case d.Status == store.DomainActive:
		return since >= recheck
	case d.Status == store.DomainPending && v.now().Sub(d.CreatedAt) > 24*time.Hour:
		return since >= recheck
	}
	// Checked every tick; the margin absorbs ticker jitter.
	return since >= orDefault(v.Interval, time.Minute)*9/10
}

// Check verifies one domain now, stores the outcome and syncs the app's
// routes when its routing changes. Checks of one domain are serialized.
func (v *Verifier) Check(ctx context.Context, d store.Domain) (store.Domain, error) {
	l := v.lock(d.Hostname)
	l.Lock()
	defer l.Unlock()

	// Re-read inside the lock: d may be stale or deleted meanwhile.
	d, err := v.Store.GetDomain(ctx, d.AppID, d.Hostname)
	if err != nil {
		return d, err
	}
	method, why := v.verifyDNS(ctx, d)
	now := v.now()
	wasRouted, wasStatus := d.Routed, d.Status
	if method != "" {
		d.VerifiedBy, d.Error, d.FailingSince, d.Routed = method, "", nil, true
		if d.VerifiedAt == nil {
			d.VerifiedAt = &now
		}
		if d.Status != store.DomainActive {
			d.Status = store.DomainVerified
		}
	} else {
		switch {
		case d.Status == store.DomainPending:
			d.Error = why
		case d.Routed:
			if d.FailingSince == nil {
				d.FailingSince = &now
			}
			until := d.FailingSince.Add(orDefault(v.Grace, 72*time.Hour))
			d.Status = store.DomainError
			if now.Before(until) {
				d.Error = why + "; still served until " + until.UTC().Format("2006-01-02 15:04 UTC")
			} else {
				d.Routed = false
				d.Error = why + "; no longer served (grace period ended)"
			}
		default:
			d.Status, d.Error = store.DomainError, why
		}
	}

	saved, err := v.Store.SaveDomainCheck(ctx, d)
	if err != nil {
		return d, err
	}
	if saved.Routed != wasRouted || saved.Status == store.DomainVerified {
		saved, err = v.applyRoute(ctx, saved)
	}
	if saved.Status != wasStatus || saved.Routed != wasRouted {
		v.Log.Info("domain checked", "app", saved.AppName, "domain", saved.Hostname,
			"status", saved.Status, "routed", saved.Routed, "via", saved.VerifiedBy)
	}
	return saved, err
}

// applyRoute syncs the app's routes and, for a verified domain, promotes
// it to active once its route exists and its certificate is ready.
func (v *Verifier) applyRoute(ctx context.Context, d store.Domain) (store.Domain, error) {
	if v.Router != nil {
		if err := v.Router.SyncApp(ctx, d.AppName); err != nil {
			if d.Status == store.DomainVerified {
				d.Error = "routing failed (retried): " + err.Error()
				d, _ = v.Store.SaveDomainCheck(ctx, d)
			}
			return d, fmt.Errorf("route sync: %w", err)
		}
	}
	if d.Status != store.DomainVerified {
		return d, nil
	}
	msg := ""
	if ok, err := v.Store.HasProduction(ctx, d.AppID); err != nil {
		return d, err
	} else if !ok {
		msg = "waiting for the first production deployment"
	} else if v.Certs != nil && v.Router != nil {
		ready, why, err := v.Certs.DomainCertificate(ctx, d.AppName, d.Hostname)
		switch {
		case err != nil:
			msg = "certificate status unknown: " + err.Error()
		case !ready:
			msg = "waiting for the certificate: " + why
		}
	}
	if msg == "" {
		d.Status = store.DomainActive
	}
	if d.Error != msg || d.Status == store.DomainActive {
		d.Error = msg
		return v.Store.SaveDomainCheck(ctx, d)
	}
	return d, nil
}

// verifyDNS returns how the domain was verified ("" if it was not) and,
// when not, why.
func (v *Verifier) verifyDNS(ctx context.Context, d store.Domain) (method, why string) {
	if v.Mode == ModeSkip {
		return ModeSkip, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	app := strings.ToLower(d.AppName + "." + v.Domain)
	apex := strings.ToLower(v.Domain)
	var reasons []string
	cname, err := v.Resolver.LookupCNAME(ctx, d.Hostname)
	cname = strings.TrimSuffix(strings.ToLower(cname), ".")
	switch {
	case err != nil:
		reasons = append(reasons, "CNAME lookup: "+lookupError(err))
	case cname == app || cname == apex:
		return "cname", ""
	case cname == d.Hostname:
		reasons = append(reasons, "no CNAME record")
	default:
		reasons = append(reasons, "CNAME points to "+cname+", not "+app)
	}

	name := ChallengePrefix + d.Hostname
	txts, err := v.Resolver.LookupTXT(ctx, name)
	if err != nil {
		reasons = append(reasons, "TXT "+name+": "+lookupError(err))
	} else {
		for _, t := range txts {
			if strings.Trim(strings.TrimSpace(t), `"`) == d.Token {
				return "txt", ""
			}
		}
		reasons = append(reasons, "TXT "+name+" does not contain the verification token")
	}
	return "", strings.Join(reasons, "; ")
}

// lookupError shortens resolver errors ("lookup x on 1.2.3.4:53: no such host").
func lookupError(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return msg
}

func (v *Verifier) lock(host string) *sync.Mutex {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.locks == nil {
		v.locks = map[string]*sync.Mutex{}
	}
	l, ok := v.locks[host]
	if !ok {
		l = &sync.Mutex{}
		v.locks[host] = l
	}
	return l
}
