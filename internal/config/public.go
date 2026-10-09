package config

import (
	"fmt"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/ratelimit"
)

// Public configures a platform open to the public (Faz 23): plan limits,
// rate limits, sign-up checks and the legal pages.
type Public struct {
	// Plans: limits of hobby, pro and unlimited. Every limit can be set
	// with PAAS_PLAN_<PLAN>_<LIMIT>, e.g. PAAS_PLAN_HOBBY_PROJECTS=5,
	// PAAS_PLAN_PRO_BUILD_MINUTES=unlimited, PAAS_PLAN_HOBBY_MEMORY=1Gi
	// (keys: plans.Keys upper-cased).
	Plans plans.Catalog
	// Rate limits, "<n>/<duration>" or "off": PAAS_RATE_LIMIT_SIGNIN
	// (per IP, default 20/1m), PAAS_RATE_LIMIT_IMPORT (creating and
	// importing apps, per user and IP, default 20/1h) and
	// PAAS_RATE_LIMIT_API_WRITES (every state-changing request, per user
	// and IP, default 120/1m).
	RateSignIn, RateImport, RateWrites ratelimit.Rule
	// TrustForwarded: PAAS_TRUST_PROXY_HEADERS=true takes the client IP
	// from X-Forwarded-For (behind Traefik); otherwise the TCP peer.
	TrustForwarded bool
	// MinAccountAge refuses open sign-ups of GitHub accounts younger than
	// this: PAAS_SIGNUP_MIN_ACCOUNT_AGE ("720h" or "30d"), default 0 = off.
	MinAccountAge time.Duration
	// ContactEmail is shown on the legal pages: PAAS_CONTACT_EMAIL.
	ContactEmail string
	// Operator names who runs the platform on the legal pages:
	// PAAS_OPERATOR_NAME, default "paas".
	Operator string
}

// LoadPublic reads the Faz 23 settings from the environment.
func LoadPublic() (Public, error) {
	p := Public{
		Plans:          plans.DefaultCatalog(),
		TrustForwarded: os.Getenv("PAAS_TRUST_PROXY_HEADERS") == "true",
		ContactEmail:   strings.TrimSpace(os.Getenv("PAAS_CONTACT_EMAIL")),
		Operator:       strings.TrimSpace(getenv("PAAS_OPERATOR_NAME", "paas")),
	}
	for _, plan := range plans.Plans {
		l := p.Plans[plan]
		for _, key := range plans.Keys {
			env := "PAAS_PLAN_" + strings.ToUpper(plan) + "_" + strings.ToUpper(key)
			v, ok := os.LookupEnv(env)
			if !ok {
				continue
			}
			if err := l.Set(key, v); err != nil {
				return p, fmt.Errorf("%s: %w", env, err)
			}
			if (key == "cpu" || key == "memory") && strings.TrimSpace(v) != "" && !plans.ValidQuantity(strings.TrimSpace(v)) {
				return p, fmt.Errorf("%s must be a Kubernetes quantity (e.g. 500m, 2, 512Mi, 2Gi)", env)
			}
		}
		p.Plans[plan] = l
	}
	for _, r := range []struct {
		env, def string
		dst      *ratelimit.Rule
	}{
		{"PAAS_RATE_LIMIT_SIGNIN", "20/1m", &p.RateSignIn},
		{"PAAS_RATE_LIMIT_IMPORT", "20/1h", &p.RateImport},
		{"PAAS_RATE_LIMIT_API_WRITES", "120/1m", &p.RateWrites},
	} {
		rule, err := ratelimit.ParseRule(getenv(r.env, r.def))
		if err != nil {
			return p, fmt.Errorf("%s: %w", r.env, err)
		}
		*r.dst = rule
	}
	if v := strings.TrimSpace(os.Getenv("PAAS_SIGNUP_MIN_ACCOUNT_AGE")); v != "" && v != "0" {
		d, err := parseAge(v)
		if err != nil {
			return p, fmt.Errorf("PAAS_SIGNUP_MIN_ACCOUNT_AGE must be a duration (720h) or days (30d)")
		}
		p.MinAccountAge = d
	}
	if p.ContactEmail != "" {
		if _, err := mail.ParseAddress(p.ContactEmail); err != nil {
			return p, fmt.Errorf("PAAS_CONTACT_EMAIL is not an e-mail address")
		}
	}
	return p, nil
}

// parseAge accepts a Go duration or a number of days ("30d").
func parseAge(v string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid days")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration")
	}
	return d, nil
}

// RateLimits builds the limiters of the settings.
func (p Public) RateLimits() *ratelimit.Set {
	return &ratelimit.Set{
		SignIn:         ratelimit.New(p.RateSignIn, nil),
		Import:         ratelimit.New(p.RateImport, nil),
		Writes:         ratelimit.New(p.RateWrites, nil),
		TrustForwarded: p.TrustForwarded,
	}
}
