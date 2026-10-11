package config

import (
	"strings"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/ratelimit"
)

func TestLoadPublicDefaults(t *testing.T) {
	p, err := LoadPublic()
	if err != nil {
		t.Fatal(err)
	}
	def := plans.DefaultCatalog()
	if p.Plans["hobby"] != def["hobby"] || p.Plans["pro"] != def["pro"] || p.Plans["unlimited"] != def["unlimited"] {
		t.Fatalf("plans %+v", p.Plans)
	}
	if p.RateSignIn != (ratelimit.Rule{Rate: 20, Per: time.Minute}) || p.RateImport != (ratelimit.Rule{Rate: 20, Per: time.Hour}) ||
		p.RateWrites != (ratelimit.Rule{Rate: 120, Per: time.Minute}) {
		t.Fatalf("rates %+v %+v %+v", p.RateSignIn, p.RateImport, p.RateWrites)
	}
	if p.MinAccountAge != 0 || p.ContactEmail != "" || p.Operator != "paas" || p.TrustForwarded {
		t.Fatalf("public %+v", p)
	}
}

func TestLoadPublicEnv(t *testing.T) {
	t.Setenv("PAAS_PLAN_HOBBY_PROJECTS", "3")
	t.Setenv("PAAS_PLAN_HOBBY_MEMORY", "1Gi")
	t.Setenv("PAAS_PLAN_PRO_BUILD_MINUTES", "unlimited")
	t.Setenv("PAAS_PLAN_HOBBY_BUILD_TIMEOUT_MINUTES", "5")
	t.Setenv("PAAS_RATE_LIMIT_SIGNIN", "5/10m")
	t.Setenv("PAAS_RATE_LIMIT_API_WRITES", "off")
	t.Setenv("PAAS_SIGNUP_MIN_ACCOUNT_AGE", "30d")
	t.Setenv("PAAS_CONTACT_EMAIL", "kvkk@example.com")
	t.Setenv("PAAS_OPERATOR_NAME", "Örnek Bulut")
	t.Setenv("PAAS_TRUST_PROXY_HEADERS", "true")
	p, err := LoadPublic()
	if err != nil {
		t.Fatal(err)
	}
	h := p.Plans["hobby"]
	if h.Projects != 3 || h.Memory != "1Gi" || h.Timeout() != 5*time.Minute || p.Plans["pro"].BuildMinutes != plans.Unlimited {
		t.Fatalf("plans %+v", p.Plans)
	}
	if p.RateSignIn != (ratelimit.Rule{Rate: 5, Per: 10 * time.Minute}) || !p.RateWrites.Off() {
		t.Fatalf("rates %+v %+v", p.RateSignIn, p.RateWrites)
	}
	if p.MinAccountAge != 30*24*time.Hour || p.ContactEmail != "kvkk@example.com" || p.Operator != "Örnek Bulut" || !p.TrustForwarded {
		t.Fatalf("public %+v", p)
	}
	set := p.RateLimits()
	if !set.TrustForwarded || set.SignIn.Rule() != p.RateSignIn || set.Writes.Rule().String() != "off" {
		t.Fatalf("limiters %+v", set)
	}
	t.Setenv("PAAS_SIGNUP_MIN_ACCOUNT_AGE", "720h")
	if p, _ := LoadPublic(); p.MinAccountAge != 720*time.Hour {
		t.Fatalf("hours: %s", p.MinAccountAge)
	}
}

func TestLoadPublicErrors(t *testing.T) {
	for env, val := range map[string]string{
		"PAAS_PLAN_HOBBY_PROJECTS":    "many",
		"PAAS_PLAN_PRO_CPU":           "lots",
		"PAAS_RATE_LIMIT_IMPORT":      "10",
		"PAAS_SIGNUP_MIN_ACCOUNT_AGE": "a month",
		"PAAS_CONTACT_EMAIL":          "not an address",
	} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, val)
			if _, err := LoadPublic(); err == nil || !strings.Contains(err.Error(), env) {
				t.Fatalf("%s=%q: %v", env, val, err)
			}
		})
	}
}
