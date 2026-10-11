// Package plans holds the limits of the team plans (Faz 23) and the Guard
// every entry point asks before it creates something that costs resources.
//
// A team is on one of three plans: hobby (the default, and the personal
// team of an open-signup user), pro and unlimited (assigned by an admin).
// The limits of each plan come from the configuration (PAAS_PLAN_<PLAN>_
// <LIMIT>, config.LoadPublic); an admin can override single limits per team.
//
// Entry points and what they check:
//
//	app create / import (API, web)            Projects
//	push webhook, deploy hook, redeploy,
//	promote (a new deployment), import        DeploymentsPerDay, BuildMinutes
//	custom domains (API, web)                 Domains
//	add-ons (API, web)                        Addons
//	worker replicas (API, web), paas.yaml     Replicas, Crons
//	worker: claim                             ConcurrentBuilds (fair queue)
//	worker: build                             BuildTimeout, Crons, Replicas
//	Kubernetes namespace                      CPU, Memory (ResourceQuota)
//
// A suspended team fails every check. A nil *Guard allows everything, so
// servers built without plans (tests, private installations) behave as
// before.
package plans

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/nisagwn/paas/internal/store"
)

// Unlimited is the value of a limit without a bound.
const Unlimited = -1

// Limits of a plan. Counts are per team; Unlimited (-1) means no bound.
type Limits struct {
	Projects          int `json:"projects"`
	ConcurrentBuilds  int `json:"concurrent_builds"`
	BuildMinutes      int `json:"build_minutes"` // per calendar month (UTC)
	BuildTimeout      int `json:"build_timeout_minutes"`
	DeploymentsPerDay int `json:"deployments_per_day"` // per calendar day (UTC)
	Domains           int `json:"domains"`
	Addons            int `json:"addons"`
	Replicas          int `json:"replicas"` // per worker process
	Crons             int `json:"crons"`    // per deployment
	// Namespace quota of each app (requests.cpu, memory) in Kubernetes
	// quantities; "" uses PAAS_APP_QUOTA_CPU / PAAS_APP_QUOTA_MEMORY.
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// Catalog maps a plan name to its limits.
type Catalog map[string]Limits

// Plans lists the plan names, cheapest first.
var Plans = []string{store.PlanHobby, store.PlanPro, store.PlanUnlimited}

// DefaultCatalog is the configuration without PAAS_PLAN_* variables. Hobby
// fits one person's side projects on a shared node; pro a company's team;
// unlimited only bounds the namespace quota by the global settings.
func DefaultCatalog() Catalog {
	return Catalog{
		store.PlanHobby: {
			Projects: 10, ConcurrentBuilds: 1, BuildMinutes: 600, BuildTimeout: 15,
			DeploymentsPerDay: 50, Domains: 3, Addons: 2, Replicas: 2, Crons: 2,
			CPU: "1", Memory: "2Gi",
		},
		store.PlanPro: {
			Projects: 100, ConcurrentBuilds: 4, BuildMinutes: 6000, BuildTimeout: 45,
			DeploymentsPerDay: 1000, Domains: 50, Addons: 20, Replicas: 10, Crons: 10,
			CPU: "4", Memory: "8Gi",
		},
		store.PlanUnlimited: {
			Projects: Unlimited, ConcurrentBuilds: Unlimited, BuildMinutes: Unlimited, BuildTimeout: Unlimited,
			DeploymentsPerDay: Unlimited, Domains: Unlimited, Addons: Unlimited, Replicas: Unlimited, Crons: Unlimited,
		},
	}
}

// Keys are the limit names, as in JSON overrides and PAAS_PLAN_<PLAN>_<KEY>
// (upper-cased).
var Keys = []string{"projects", "concurrent_builds", "build_minutes", "build_timeout_minutes",
	"deployments_per_day", "domains", "addons", "replicas", "crons", "cpu", "memory"}

// Set changes one limit by key. Numbers accept "unlimited" or -1; "cpu"
// and "memory" take a Kubernetes quantity ("" = the global quota).
func (l *Limits) Set(key, value string) error {
	value = strings.TrimSpace(value)
	switch key {
	case "cpu":
		l.CPU = value
		return nil
	case "memory":
		l.Memory = value
		return nil
	}
	dst := l.intField(key)
	if dst == nil {
		return fmt.Errorf("unknown limit %q", key)
	}
	if strings.EqualFold(value, "unlimited") {
		*dst = Unlimited
		return nil
	}
	var n int
	if _, err := fmt.Sscanf(value, "%d", &n); err != nil || fmt.Sprint(n) != value || n < Unlimited {
		return fmt.Errorf("limit %s must be a non-negative integer or \"unlimited\"", key)
	}
	*dst = n
	return nil
}

func (l *Limits) intField(key string) *int {
	switch key {
	case "projects":
		return &l.Projects
	case "concurrent_builds":
		return &l.ConcurrentBuilds
	case "build_minutes":
		return &l.BuildMinutes
	case "build_timeout_minutes":
		return &l.BuildTimeout
	case "deployments_per_day":
		return &l.DeploymentsPerDay
	case "domains":
		return &l.Domains
	case "addons":
		return &l.Addons
	case "replicas":
		return &l.Replicas
	case "crons":
		return &l.Crons
	}
	return nil
}

// Timeout is the build timeout; 0 means none.
func (l Limits) Timeout() time.Duration {
	if l.BuildTimeout <= 0 {
		return 0
	}
	return time.Duration(l.BuildTimeout) * time.Minute
}

// Resolve applies a team's overrides to the limits of its plan. Unknown
// keys and malformed values are ignored (ValidateOverrides refuses them
// before they are stored).
func (c Catalog) Resolve(plan string, overrides json.RawMessage) Limits {
	l, ok := c[plan]
	if !ok {
		l = c[store.PlanHobby]
	}
	if len(overrides) == 0 {
		return l
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(overrides, &m) != nil {
		return l
	}
	for k, v := range m {
		var s string
		if json.Unmarshal(v, &s) != nil {
			s = string(v) // a number
		}
		_ = l.Set(k, s)
	}
	return l
}

// ValidateOverrides checks an admin's overrides and returns them in their
// stored form: numbers for counts (-1 unlimited), strings for quantities.
func ValidateOverrides(in map[string]any) (json.RawMessage, error) {
	out := map[string]any{}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var probe Limits
		v := in[k]
		var s string
		switch x := v.(type) {
		case string:
			s = x
		case float64:
			s = fmt.Sprint(int64(x))
			if float64(int64(x)) != x {
				return nil, fmt.Errorf("limit %s must be a non-negative integer or \"unlimited\"", k)
			}
		case nil:
			continue // null: follow the plan
		default:
			return nil, fmt.Errorf("limit %s must be a number or a string", k)
		}
		if err := probe.Set(k, s); err != nil {
			return nil, err
		}
		switch k {
		case "cpu":
			if probe.CPU != "" && !ValidQuantity(probe.CPU) {
				return nil, fmt.Errorf("limit cpu must be a Kubernetes quantity such as \"500m\" or \"2\"")
			}
			out[k] = probe.CPU
		case "memory":
			if probe.Memory != "" && !ValidQuantity(probe.Memory) {
				return nil, fmt.Errorf("limit memory must be a Kubernetes quantity such as \"512Mi\" or \"2Gi\"")
			}
			out[k] = probe.Memory
		default:
			out[k] = *probe.intField(k)
		}
	}
	b, err := json.Marshal(out)
	return json.RawMessage(b), err
}

// ValidQuantity accepts a positive Kubernetes quantity ("500m", "2Gi").
func ValidQuantity(q string) bool {
	v, err := resource.ParseQuantity(q)
	return err == nil && v.Sign() > 0
}
