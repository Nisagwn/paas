package process

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidSchedule(t *testing.T) {
	ok := []string{
		"* * * * *", "*/15 * * * *", "0 3 * * *", "0 0 1 * *", "30 2 * * mon-fri", "0 9 * JAN,jul sun",
		"0-30/5 8-18 * * 1-5", "5,10,15 * * * *", "0 0 ? * *", "@hourly", "@daily", "@Weekly", "@midnight",
		"  0 12 * * 0  ", "59 23 31 12 6", "5/10 * * * *",
	}
	for _, s := range ok {
		if err := ValidSchedule(s); err != nil {
			t.Errorf("ValidSchedule(%q) = %v", s, err)
		}
	}
	bad := map[string]string{
		"":               "empty",
		"* * * *":        "5 fields",
		"* * * * * *":    "5 fields",
		"60 * * * *":     "outside 0-59",
		"* 24 * * *":     "outside 0-23",
		"* * 0 * *":      "outside 1-31",
		"* * * 13 *":     "outside 1-12",
		"* * * * 7":      "outside 0-6",
		"*/0 * * * *":    "step",
		"*/61 * * * *":   "step",
		"10-5 * * * *":   "after its end",
		"a * * * *":      "not a value",
		"? * * * *":      "not a value",
		"* * * foo *":    "not a value",
		"-1 * * * *":     "not a value",
		"@every 5m":      "unknown macro",
		"TZ=UTC * * * *": "not a value",
		"1,,2 * * * *":   "not a value",
	}
	for s, want := range bad {
		err := ValidSchedule(s)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidSchedule(%q) = %v, want %q", s, err, want)
		}
	}
}

func TestActivity(t *testing.T) {
	s := Set{
		Workers: []Worker{
			{Name: "queue", Replicas: 2, Previews: true},
			{Name: "bot", Replicas: 1},
			{Name: "idle", Replicas: 0, Previews: true},
		},
		Crons: []Cron{{Name: "report"}, {Name: "warm", Previews: true}},
	}
	tests := []struct {
		role      Role
		overrides map[string]int
		workers   map[string]int32
		crons     map[string]bool
	}{
		{RoleProduction, nil,
			map[string]int32{"queue": 2, "bot": 1, "idle": 0}, map[string]bool{"report": true, "warm": true}},
		{RoleProduction, map[string]int{"queue": 5, "bot": 0, "gone": 3},
			map[string]int32{"queue": 5, "bot": 0, "idle": 0}, map[string]bool{"report": true, "warm": true}},
		// Overrides are production scaling; previews use the file.
		{RolePreview, map[string]int{"queue": 5},
			map[string]int32{"queue": 2, "bot": 0, "idle": 0}, map[string]bool{"report": false, "warm": true}},
		{RoleNone, map[string]int{"queue": 5},
			map[string]int32{"queue": 0, "bot": 0, "idle": 0}, map[string]bool{"report": false, "warm": false}},
	}
	for _, tt := range tests {
		a := s.Activity(tt.role, tt.overrides)
		if !reflect.DeepEqual(a.Workers, tt.workers) || !reflect.DeepEqual(a.Crons, tt.crons) {
			t.Errorf("role %d: got %v %v, want %v %v", tt.role, a.Workers, a.Crons, tt.workers, tt.crons)
		}
	}
}

func TestPlanOneGeneration(t *testing.T) {
	set := Set{Workers: []Worker{{Name: "w", Replicas: 1, Previews: true}}, Crons: []Cron{{Name: "c"}}}
	deps := []Deployment{
		{ID: 1, Ready: true, Set: set},                                  // old production: stopped
		{ID: 2, Ready: true, Production: true, Preview: true, Set: set}, // current production
		{ID: 3, Ready: true, Preview: true, Set: set},                   // branch preview
		{ID: 4, InFlight: true, Set: set},                               // being deployed: untouched
		{ID: 5, Ready: false, Production: true, Set: set},               // failed: stopped
	}
	p := PlanFor(deps, map[string]int{"w": 3})
	cases := []struct {
		id       int64
		replicas int32
		cron     bool
		leave    bool
	}{
		{1, 0, false, false},
		{2, 3, true, false},
		{3, 1, false, false},
		{4, 0, false, true},
		{5, 0, false, false},
		{99, 0, false, false}, // unknown deployment: stopped
	}
	for _, c := range cases {
		if got := p.Replicas(c.id, "w"); got != c.replicas {
			t.Errorf("deployment %d: replicas %d, want %d", c.id, got, c.replicas)
		}
		if got := p.CronActive(c.id, "c"); got != c.cron {
			t.Errorf("deployment %d: cron active %v, want %v", c.id, got, c.cron)
		}
		if got := p.Leave(c.id); got != c.leave {
			t.Errorf("deployment %d: leave %v, want %v", c.id, got, c.leave)
		}
	}
	// Rollback: production moves back to 1; 2 stops.
	deps[0].Production, deps[1].Production = true, false
	deps[1].Preview = false
	p = PlanFor(deps, nil)
	if p.Replicas(1, "w") != 1 || !p.CronActive(1, "c") || p.Replicas(2, "w") != 0 || p.CronActive(2, "c") {
		t.Errorf("after rollback: %+v %+v", *p[1], *p[2])
	}
}

func TestNames(t *testing.T) {
	s := Set{Workers: []Worker{{Name: "a"}}, Crons: []Cron{{Name: "c"}}}
	if got := s.Names(); !reflect.DeepEqual(got, []string{"web", "a", "c"}) {
		t.Errorf("Names = %q", got)
	}
	s.NoWeb = true
	if got := s.Names(); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("Names without web = %q", got)
	}
	if !Default().Empty() || s.Empty() {
		t.Error("Empty")
	}
}
