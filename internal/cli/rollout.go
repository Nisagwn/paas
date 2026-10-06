package cli

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Faz 21: canary / guarded rollouts.
//
//	paas rollout status <app>
//	paas rollout promote|abort|pause|resume|rollback <app>
//	paas rollout settings <app> [--mode canary] [--steps 10,50,100] [--step 5m] ...

type rolloutSettings struct {
	Mode                string  `json:"mode"`
	Steps               []int   `json:"steps"`
	StepSeconds         int     `json:"step_seconds"`
	MaxErrorPct         float64 `json:"max_error_pct"`
	MaxErrorIncreasePct float64 `json:"max_error_increase_pct"`
	MaxP95Ms            int     `json:"max_p95_ms"`
	MaxP95Factor        float64 `json:"max_p95_factor"`
	MinRequests         int     `json:"min_requests"`
	GuardSeconds        int     `json:"guard_seconds"`
}

type rolloutSample struct {
	Requests int64    `json:"requests"`
	Errors   int64    `json:"errors"`
	P95Ms    *float64 `json:"p95_ms"`
}

type rolloutVerdict struct {
	At     time.Time `json:"at"`
	Weight int       `json:"weight"`
	Action string    `json:"action"`
	Reason string    `json:"reason"`
}

type rolloutInfo struct {
	ID               int64            `json:"id"`
	FromDeploymentID *int64           `json:"from_deployment_id"`
	ToDeploymentID   int64            `json:"to_deployment_id"`
	Mode             string           `json:"mode"`
	Step             int              `json:"step"`
	Weight           int              `json:"weight"`
	State            string           `json:"state"`
	Reason           string           `json:"reason"`
	Settings         rolloutSettings  `json:"settings"`
	Verdicts         []rolloutVerdict `json:"verdicts"`
	CreatedAt        time.Time        `json:"created_at"`
	FinishedAt       *time.Time       `json:"finished_at"`
	ToURL            string           `json:"to_url"`
	Metrics          *struct {
		Canary     rolloutSample `json:"canary"`
		Production rolloutSample `json:"production"`
		Next       string        `json:"next_action"`
		NextWhy    string        `json:"next_reason"`
		Elapsed    int64         `json:"elapsed_seconds"`
		Duration   int64         `json:"duration_seconds"`
	} `json:"metrics"`
}

type rolloutStatus struct {
	Settings rolloutSettings `json:"settings"`
	Active   *rolloutInfo    `json:"active"`
	Recent   []rolloutInfo   `json:"recent"`
}

func stepsText(steps []int) string {
	parts := make([]string, len(steps))
	for i, w := range steps {
		parts[i] = strconv.Itoa(w)
	}
	return strings.Join(parts, ",")
}

func (s rolloutSample) text() string {
	if s.Requests == 0 {
		return "no requests"
	}
	out := fmt.Sprintf("%d requests, 5xx %.1f%%", s.Requests, 100*float64(s.Errors)/float64(s.Requests))
	if s.P95Ms != nil {
		out += fmt.Sprintf(", p95 %.0f ms", *s.P95Ms)
	}
	return out
}

func cmdRollout(r *runner, args []string) error {
	fs := r.flags()
	var set rolloutFlags
	set.register(fs)
	pos, err := r.parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	sub, appName := pos[0], pos[1]
	changed := set.changed(fs)
	switch sub {
	case "status", "promote", "abort", "pause", "resume", "rollback":
		if len(changed) > 0 {
			return usagef("settings flags only apply to `paas rollout settings`")
		}
	case "settings":
	default:
		return usagef("unknown rollout subcommand %q (want status, promote, abort, pause, resume, rollback or settings)", sub)
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	switch sub {
	case "status":
		var st rolloutStatus
		raw, err := c.Do(r.ctx, "GET", appPath(appName, "rollout"), nil, &st)
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		return r.printRolloutStatus(appName, st)
	case "settings":
		var s rolloutSettings
		var raw []byte
		if len(changed) == 0 {
			raw, err = c.Do(r.ctx, "GET", appPath(appName, "rollout-settings"), nil, &s)
		} else {
			body, berr := set.body(changed)
			if berr != nil {
				return berr
			}
			raw, err = c.Do(r.ctx, "PUT", appPath(appName, "rollout-settings"), body, &s)
		}
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		printRolloutSettings(r, appName, s)
		return nil
	}
	var ro rolloutInfo
	raw, err := c.Do(r.ctx, "POST", appPath(appName, "rollout", sub), nil, &ro)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	fmt.Fprintf(r.Stdout, "Rollout #%d of %s (deployment #%d): %s, weight %d%%.\n", ro.ID, appName, ro.ToDeploymentID,
		ro.State, ro.Weight)
	return nil
}

func printRolloutSettings(r *runner, app string, s rolloutSettings) {
	tw := newTable(r.Stdout)
	fmt.Fprintf(tw, "App:\t%s\n", app)
	fmt.Fprintf(tw, "Mode:\t%s\n", s.Mode)
	fmt.Fprintf(tw, "Canary steps:\t%s%% (%s each)\n", stepsText(s.Steps), duration(time.Duration(s.StepSeconds)*time.Second))
	fmt.Fprintf(tw, "Guard window:\t%s\n", duration(time.Duration(s.GuardSeconds)*time.Second))
	fmt.Fprintf(tw, "Max 5xx:\t%g%% (at most %g points above production)\n", s.MaxErrorPct, s.MaxErrorIncreasePct)
	p95 := "off"
	if s.MaxP95Ms > 0 {
		p95 = fmt.Sprintf("%d ms", s.MaxP95Ms)
	}
	factor := "off"
	if s.MaxP95Factor > 0 {
		factor = fmt.Sprintf("%g× production", s.MaxP95Factor)
	}
	fmt.Fprintf(tw, "Max p95:\t%s, %s\n", p95, factor)
	fmt.Fprintf(tw, "Min requests:\t%d per step\n", s.MinRequests)
	tw.Flush()
}

func (r *runner) printRolloutStatus(app string, st rolloutStatus) error {
	a := st.Active
	if a == nil {
		fmt.Fprintf(r.Stdout, "No rollout in progress for %s (mode %s).\n", app, st.Settings.Mode)
	} else {
		tw := newTable(r.Stdout)
		fmt.Fprintf(tw, "Rollout:\t#%d %s, %s\n", a.ID, a.Mode, a.State)
		from := "-"
		if a.FromDeploymentID != nil {
			from = fmt.Sprintf("#%d", *a.FromDeploymentID)
		}
		fmt.Fprintf(tw, "Deployments:\t#%d (new) ← %s (production)\n", a.ToDeploymentID, from)
		if a.Mode == "canary" {
			fmt.Fprintf(tw, "Weight:\t%d%% (step %d of %s%%)\n", a.Weight, a.Step+1, stepsText(a.Settings.Steps))
		}
		if m := a.Metrics; m != nil {
			fmt.Fprintf(tw, "New:\t%s\n", m.Canary.text())
			fmt.Fprintf(tw, "Production:\t%s\n", m.Production.text())
			fmt.Fprintf(tw, "Window:\t%s of %s\n", duration(time.Duration(m.Elapsed)*time.Second),
				duration(time.Duration(m.Duration)*time.Second))
			fmt.Fprintf(tw, "Next:\t%s: %s\n", m.Next, m.NextWhy)
		}
		if a.Reason != "" {
			fmt.Fprintf(tw, "Last:\t%s\n", a.Reason)
		}
		tw.Flush()
	}
	var done []rolloutInfo
	for _, ro := range st.Recent {
		if a == nil || ro.ID != a.ID {
			done = append(done, ro)
		}
	}
	if len(done) == 0 {
		return nil
	}
	fmt.Fprintln(r.Stdout, "\nRecent:")
	tw := newTable(r.Stdout)
	fmt.Fprintln(tw, "ID\tDEPLOYMENT\tMODE\tSTATE\tFINISHED\tREASON")
	for _, ro := range done {
		finished := "-"
		if ro.FinishedAt != nil {
			finished = relTime(r.Now(), *ro.FinishedAt)
		}
		fmt.Fprintf(tw, "%d\t#%d\t%s\t%s\t%s\t%s\n", ro.ID, ro.ToDeploymentID, ro.Mode, ro.State, finished, orDash(oneLine(ro.Reason, 70)))
	}
	return tw.Flush()
}

// rolloutFlags are the settings flags of `paas rollout settings`.
type rolloutFlags struct {
	mode, steps                      string
	step, guard                      time.Duration
	maxError, maxIncrease, p95Factor float64
	p95Ms, minRequests               int
}

func (f *rolloutFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.mode, "mode", "", "`instant`, guarded or canary")
	fs.StringVar(&f.steps, "steps", "", "canary weights in percent, e.g. `10,50,100`")
	fs.DurationVar(&f.step, "step", 0, "duration of one canary step (≥ 2m)")
	fs.DurationVar(&f.guard, "guard", 0, "watch window of guarded mode (≥ 2m)")
	fs.Float64Var(&f.maxError, "max-error-pct", 0, "highest 5xx rate of the new deployment (percent)")
	fs.Float64Var(&f.maxIncrease, "max-error-increase", 0, "most the 5xx rate may exceed production's (points)")
	fs.IntVar(&f.p95Ms, "max-p95-ms", 0, "p95 latency ceiling in ms (0: off)")
	fs.Float64Var(&f.p95Factor, "max-p95-factor", 0, "p95 ceiling as a factor of production's (0: off)")
	fs.IntVar(&f.minRequests, "min-requests", 0, "requests a step needs before rates count")
}

var rolloutFlagNames = map[string]bool{"mode": true, "steps": true, "step": true, "guard": true, "max-error-pct": true,
	"max-error-increase": true, "max-p95-ms": true, "max-p95-factor": true, "min-requests": true}

func (f *rolloutFlags) changed(fs *flag.FlagSet) map[string]bool {
	out := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) {
		if rolloutFlagNames[fl.Name] {
			out[fl.Name] = true
		}
	})
	return out
}

// body is the PUT body with only the flags given.
func (f *rolloutFlags) body(changed map[string]bool) (map[string]any, error) {
	b := map[string]any{}
	if changed["mode"] {
		b["mode"] = f.mode
	}
	if changed["steps"] {
		var steps []int
		for _, p := range strings.Split(f.steps, ",") {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(p), "%"))
			if err != nil {
				return nil, usagef("--steps must be comma-separated percentages, e.g. 10,50,100")
			}
			steps = append(steps, n)
		}
		b["steps"] = steps
	}
	if changed["step"] {
		b["step_seconds"] = int(f.step.Seconds())
	}
	if changed["guard"] {
		b["guard_seconds"] = int(f.guard.Seconds())
	}
	if changed["max-error-pct"] {
		b["max_error_pct"] = f.maxError
	}
	if changed["max-error-increase"] {
		b["max_error_increase_pct"] = f.maxIncrease
	}
	if changed["max-p95-ms"] {
		b["max_p95_ms"] = f.p95Ms
	}
	if changed["max-p95-factor"] {
		b["max_p95_factor"] = f.p95Factor
	}
	if changed["min-requests"] {
		b["min_requests"] = f.minRequests
	}
	return b, nil
}
