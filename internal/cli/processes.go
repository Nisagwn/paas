package cli

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Faz 20: process types.
//
//	paas ps <app> [--deployment ID]          web, workers and crons with their state
//	paas ps scale <app> NAME=N [NAME=N...]   worker replicas (N=default: back to paas.yaml)
//	paas cron run <app> <name>               start a cron job now
//	paas logs --runtime --process NAME <app> <deployment-id>

type processView struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Command  string `json:"command"`
	Replicas int    `json:"replicas"`
	Override *int   `json:"override"`
	Previews bool   `json:"previews"`
	Desired  int32  `json:"desired"`
	Ready    int32  `json:"ready"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
}

type jobView struct {
	Name     string     `json:"name"`
	Status   string     `json:"status"`
	Manual   bool       `json:"manual"`
	Started  *time.Time `json:"started"`
	Finished *time.Time `json:"finished"`
}

type cronView struct {
	Name             string     `json:"name"`
	Schedule         string     `json:"schedule"`
	Command          string     `json:"command"`
	Exists           bool       `json:"exists"`
	Suspended        bool       `json:"suspended"`
	Running          int        `json:"running"`
	LastScheduleTime *time.Time `json:"last_schedule_time"`
	LastJob          *jobView   `json:"last_job"`
}

type processesView struct {
	DeploymentID int64         `json:"deployment_id"`
	Production   bool          `json:"production"`
	Source       string        `json:"source"`
	Processes    []processView `json:"processes"`
	Crons        []cronView    `json:"crons"`
	StatusError  string        `json:"status_error"`
}

func cmdPs(r *runner, args []string) error {
	fs := r.flags()
	depID := fs.String("deployment", "", "show this deployment instead of production")
	pos, err := r.parse(fs, args, 1, -1)
	if err != nil {
		return err
	}
	if pos[0] == "scale" && len(pos) > 1 {
		if *depID != "" {
			return usagef("--deployment does not apply to scale: replicas are set for production")
		}
		return r.psScale(pos[1], pos[2:])
	}
	if len(pos) > 1 {
		return usagef("too many arguments: %s", strings.Join(pos[1:], " "))
	}
	path := appPath(pos[0], "processes")
	if *depID != "" {
		id, err := parseID(*depID)
		if err != nil {
			return err
		}
		path += "?deployment=" + strconv.FormatInt(id, 10)
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	var v processesView
	raw, err := c.Do(r.ctx, "GET", path, nil, &v)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	r.printProcesses(v)
	return nil
}

func (r *runner) printProcesses(v processesView) {
	where := "not production"
	if v.Production {
		where = "production"
	}
	source := v.Source
	if source == "" {
		source = "no paas.yaml or Procfile: web only"
	}
	fmt.Fprintf(r.Stdout, "Deployment #%d (%s) · %s\n", v.DeploymentID, where, source)
	if v.StatusError != "" {
		fmt.Fprintln(r.Stderr, "Warning:", v.StatusError)
	}
	fmt.Fprintln(r.Stdout)
	tw := newTable(r.Stdout)
	fmt.Fprintln(tw, "PROCESS\tTYPE\tREADY\tSTATE\tCOMMAND")
	for _, p := range v.Processes {
		state := p.State
		if p.Reason != "" {
			state += " (" + p.Reason + ")"
		}
		cmd := p.Command
		if cmd == "" && p.Type == "web" {
			cmd = "(detected start command)"
		}
		ready := fmt.Sprintf("%d/%d", p.Ready, p.Desired)
		if p.Override != nil {
			ready += fmt.Sprintf(" (set: %d, paas.yaml: %d)", *p.Override, p.Replicas)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.Type, ready, orDash(state), oneLine(cmd, 60))
	}
	tw.Flush()
	if len(v.Crons) == 0 {
		return
	}
	now := r.Now()
	fmt.Fprintln(r.Stdout)
	tw = newTable(r.Stdout)
	fmt.Fprintln(tw, "CRON\tSCHEDULE (UTC)\tLAST RUN\tSTATUS\tCOMMAND")
	for _, c := range v.Crons {
		last, status := "-", "scheduled"
		switch {
		case !c.Exists:
			status = "missing"
		case c.Suspended:
			status = "suspended"
		}
		if j := c.LastJob; j != nil {
			status = j.Status
			if j.Started != nil {
				last = relTime(now, *j.Started)
			}
			if j.Manual {
				last += " (manual)"
			}
		} else if c.LastScheduleTime != nil {
			last = relTime(now, *c.LastScheduleTime)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Name, c.Schedule, last, status, oneLine(c.Command, 60))
	}
	tw.Flush()
}

// psScale applies NAME=N pairs one by one, in name order.
func (r *runner) psScale(appName string, pairs []string) error {
	if len(pairs) == 0 {
		return usagef("give at least one NAME=N, e.g. worker=2")
	}
	want := map[string]*int{}
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return usagef("%q is not NAME=N", kv)
		}
		if v == "default" {
			want[k] = nil
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 10 {
			return usagef("%q: replicas must be 0-10 (or \"default\" for the paas.yaml value)", kv)
		}
		want[k] = &n
	}
	names := make([]string, 0, len(want))
	for k := range want {
		names = append(names, k)
	}
	sort.Strings(names)
	c, err := r.client()
	if err != nil {
		return err
	}
	var v processesView
	var raw []byte
	for _, name := range names {
		raw, err = c.Do(r.ctx, "PUT", appPath(appName, "processes", url.PathEscape(name)),
			map[string]*int{"replicas": want[name]}, &v)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if !r.g.JSON {
			if want[name] == nil {
				fmt.Fprintf(r.Stdout, "Scaled %s back to its paas.yaml replicas.\n", name)
			} else {
				fmt.Fprintf(r.Stdout, "Scaled %s to %d replica(s).\n", name, *want[name])
			}
		}
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	fmt.Fprintln(r.Stdout)
	r.printProcesses(v)
	return nil
}

func cmdCron(r *runner, args []string) error {
	pos, err := r.parse(r.flags(), args, 3, 3)
	if err != nil {
		return err
	}
	if pos[0] != "run" {
		return usagef("unknown cron subcommand %q (want run)", pos[0])
	}
	appName, cron := pos[1], pos[2]
	c, err := r.client()
	if err != nil {
		return err
	}
	var res struct {
		Job string `json:"job"`
	}
	raw, err := c.Do(r.ctx, "POST", appPath(appName, "crons", url.PathEscape(cron), "run"), nil, &res)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	fmt.Fprintf(r.Stdout, "Started %s of %s (job %s).\nFollow it with: paas ps %s, then paas logs --runtime --process %s %s <deployment-id>\n",
		cron, appName, res.Job, appName, cron, appName)
	return nil
}
