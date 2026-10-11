package plans_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/plans"
	"github.com/nisagwn/paas/internal/store"
)

func TestLimitsSet(t *testing.T) {
	var l plans.Limits
	for key, val := range map[string]string{"projects": "5", "build_minutes": "unlimited", "domains": "-1",
		"build_timeout_minutes": "20", "cpu": "500m", "memory": " 2Gi "} {
		if err := l.Set(key, val); err != nil {
			t.Fatalf("Set(%s, %s): %v", key, val, err)
		}
	}
	if l.Projects != 5 || l.BuildMinutes != plans.Unlimited || l.Domains != plans.Unlimited ||
		l.Timeout() != 20*time.Minute || l.CPU != "500m" || l.Memory != "2Gi" {
		t.Fatalf("limits %+v", l)
	}
	for key, val := range map[string]string{"projects": "five", "addons": "-2", "replicas": "1.5", "nope": "1"} {
		if err := l.Set(key, val); err == nil {
			t.Errorf("Set(%s, %s) accepted", key, val)
		}
	}
	if (plans.Limits{BuildTimeout: plans.Unlimited}).Timeout() != 0 {
		t.Error("unlimited timeout")
	}
}

func TestResolveOverrides(t *testing.T) {
	c := plans.DefaultCatalog()
	l := c.Resolve(store.PlanHobby, json.RawMessage(`{"projects": 50, "memory": "4Gi", "domains": -1, "bogus": 3}`))
	want := c[store.PlanHobby]
	want.Projects, want.Memory, want.Domains = 50, "4Gi", plans.Unlimited
	if l != want {
		t.Fatalf("resolved %+v, want %+v", l, want)
	}
	if c.Resolve("gold", nil) != c[store.PlanHobby] {
		t.Fatal("unknown plan does not fall back to hobby")
	}
	if c.Resolve(store.PlanPro, json.RawMessage(`not json`)) != c[store.PlanPro] {
		t.Fatal("malformed overrides")
	}
	// Hobby is the tightest plan, unlimited has no bounds.
	h, p, u := c[store.PlanHobby], c[store.PlanPro], c[store.PlanUnlimited]
	if h.Projects >= p.Projects || h.ConcurrentBuilds >= p.ConcurrentBuilds || u.Projects != plans.Unlimited || u.Timeout() != 0 {
		t.Fatalf("catalog %+v", c)
	}
}

func TestValidateOverrides(t *testing.T) {
	raw, err := plans.ValidateOverrides(map[string]any{"projects": float64(50), "build_minutes": "unlimited",
		"memory": "4Gi", "domains": nil})
	if err != nil || string(raw) != `{"build_minutes":-1,"memory":"4Gi","projects":50}` {
		t.Fatalf("overrides %s %v", raw, err)
	}
	for name, in := range map[string]map[string]any{
		"unknown key":  {"gpus": float64(1)},
		"fraction":     {"projects": 1.5},
		"negative":     {"projects": float64(-3)},
		"bad memory":   {"memory": "lots"},
		"zero cpu":     {"cpu": "0"},
		"bool":         {"projects": true},
		"word for int": {"addons": "many"},
	} {
		if _, err := plans.ValidateOverrides(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if !plans.ValidQuantity("500m") || plans.ValidQuantity("-1") || plans.ValidQuantity("x") {
		t.Error("ValidQuantity")
	}
}

// A nil Guard allows everything (servers without plans).
func TestNilGuard(t *testing.T) {
	var g *plans.Guard
	ctx := t.Context()
	for name, err := range map[string]error{
		"create": g.CheckCreateApp(ctx, 1), "deploy": g.CheckDeploy(ctx, 1), "deploy app": g.CheckDeployApp(ctx, 1),
		"domain": g.CheckDomain(ctx, 1), "addon": g.CheckAddon(ctx, 1), "replicas": g.CheckReplicas(ctx, 1, 99),
	} {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if d, err := g.BuildTimeout(ctx, 1); d != 0 || err != nil {
		t.Errorf("timeout %s %v", d, err)
	}
	if caps := g.ClaimCaps(); caps.ByPlan != nil {
		t.Errorf("caps %+v", caps)
	}
	if plans.AsLimit(nil) != nil {
		t.Error("AsLimit(nil)")
	}
	if plans.Title(store.PlanPro) != "Pro" || plans.Title("x") != "x" {
		t.Error("Title")
	}
}
