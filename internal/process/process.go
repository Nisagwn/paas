// Package process describes what a deployment runs besides (or instead of)
// its web server (Faz 20): long-running workers and scheduled cron jobs, all
// from the same image as the web process.
//
// The set comes from the repository (config.go): paas.yaml (or paas.yml,
// paas.json) wins over a Procfile; without either a deployment runs only its
// web process. The builder reads it from the commit it builds, so a
// deployment's processes are as immutable as its image.
//
// Only one generation of workers and crons runs per app environment
// (Activity): two immutable deployments must not consume the same queue or
// fire the same schedule. Processes run for the deployment the production
// alias points at; the target of a branch preview alias runs only the
// processes marked previews: true (and only if it is a preview-environment
// deployment: the production branch's own preview alias does not start a
// second production generation); every other deployment keeps its workers
// at zero replicas and its crons suspended.
package process

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Limits of a process set.
const (
	MaxWorkers    = 10
	MaxCrons      = 10
	MaxReplicas   = 10
	MaxCommandLen = 1024
	// MaxNameLen keeps object names (<deployment>-w-<name>) well inside
	// Kubernetes' limits.
	MaxNameLen = 20
)

// Web is the name of the HTTP process. It is never a worker or cron name.
const Web = "web"

// Kinds of processes, as reported by the API and set as object labels.
const (
	KindWeb    = "web"
	KindWorker = "worker"
	KindCron   = "cron"
)

// Sources of a process set.
const (
	SourceDefault  = ""         // no file: web only
	SourceProcfile = "Procfile" // Heroku-style Procfile
)

// Set is the process set of one deployment. It is stored as JSON with the
// deployment (store/processes.go).
type Set struct {
	// Source is the file the set was read from: paas.yaml, paas.yml,
	// paas.json, Procfile or "" (none).
	Source string `json:"source,omitempty"`
	// NoWeb: the deployment has no HTTP process (web: none, or no start
	// command could be detected for an app with workers). It gets no
	// Deployment, Service or Ingress of its own; it is ready once its
	// workers are.
	NoWeb bool `json:"no_web,omitempty"`
	// WebCommand is the web command as written in the file, for display.
	// Empty: the start command the build detected.
	WebCommand string `json:"web_command,omitempty"`
	// WebExec replaces the image's command for the web container. The
	// builder sets it only when the command cannot be baked into a
	// generated Dockerfile (a Dockerfile project, or a command list).
	WebExec []string `json:"web_exec,omitempty"`
	Workers []Worker `json:"workers,omitempty"`
	Crons   []Cron   `json:"crons,omitempty"`
}

// Worker is a long-running process without a port: a queue consumer, a
// chat bot, a WebSocket fan-out, anything that runs until stopped.
type Worker struct {
	Name string `json:"name"`
	// Command as written, for display.
	Command string `json:"command"`
	// Exec is the container command (Resolve): ["sh", "-c", Command] in
	// images with a shell, the command split on spaces otherwise, or the
	// list given in paas.yaml.
	Exec []string `json:"exec,omitempty"`
	// Replicas from the file (default 1). An app's override wins
	// (store.SetProcessReplicas).
	Replicas int `json:"replicas"`
	// Previews: also run for the target of a branch preview alias.
	Previews bool `json:"previews,omitempty"`
}

// Cron is a command run on a schedule (Kubernetes CronJob, UTC).
type Cron struct {
	Name     string   `json:"name"`
	Schedule string   `json:"schedule"`
	Command  string   `json:"command"`
	Exec     []string `json:"exec,omitempty"`
	Previews bool     `json:"previews,omitempty"`
}

// Default is the set of a deployment without process configuration (and
// of every deployment built before Faz 20): its web process only.
func Default() Set { return Set{} }

// HasWeb reports whether the deployment runs an HTTP process.
func (s Set) HasWeb() bool { return !s.NoWeb }

// Worker returns the worker called name.
func (s Set) Worker(name string) (Worker, bool) {
	for _, w := range s.Workers {
		if w.Name == name {
			return w, true
		}
	}
	return Worker{}, false
}

// Cron returns the cron job called name.
func (s Set) Cron(name string) (Cron, bool) {
	for _, c := range s.Crons {
		if c.Name == name {
			return c, true
		}
	}
	return Cron{}, false
}

// Names lists every process of the set: "web" (when there is one), the
// workers and the crons, in that order.
func (s Set) Names() []string {
	var out []string
	if s.HasWeb() {
		out = append(out, Web)
	}
	for _, w := range s.Workers {
		out = append(out, w.Name)
	}
	for _, c := range s.Crons {
		out = append(out, c.Name)
	}
	return out
}

// Empty: nothing but the web process.
func (s Set) Empty() bool { return s.HasWeb() && len(s.Workers) == 0 && len(s.Crons) == 0 }

// Resolve fills in the container command of every worker and cron that
// does not have one. shell: the image has /bin/sh (every generated image
// but Go's distroless one); without a shell a command is split on spaces
// and run directly, like a Go start command.
func (s *Set) Resolve(shell bool) {
	exec := func(cmd string) []string {
		if shell {
			return []string{"sh", "-c", cmd}
		}
		return strings.Fields(cmd)
	}
	for i := range s.Workers {
		if len(s.Workers[i].Exec) == 0 {
			s.Workers[i].Exec = exec(s.Workers[i].Command)
		}
	}
	for i := range s.Crons {
		if len(s.Crons[i].Exec) == 0 {
			s.Crons[i].Exec = exec(s.Crons[i].Command)
		}
	}
}

var nameRe = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// ValidName reports whether name can name a worker or cron: a DNS label of
// at most MaxNameLen characters, lowercase, starting with a letter.
func ValidName(name string) bool {
	return len(name) <= MaxNameLen && nameRe.MatchString(name)
}

func checkName(what, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("%s name %q must be 1-%d characters: lowercase letters, digits and '-', "+
			"starting with a letter and not ending with '-'", what, name, MaxNameLen)
	}
	return nil
}

// checkCommand keeps commands on one line of printable characters: a web
// command ends up in a Dockerfile, and one rule for all is easier to learn.
func checkCommand(what, cmd string) error {
	switch {
	case strings.TrimSpace(cmd) == "":
		return fmt.Errorf("%s: command is empty", what)
	case len(cmd) > MaxCommandLen:
		return fmt.Errorf("%s: command is longer than %d characters", what, MaxCommandLen)
	}
	for _, r := range cmd {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("%s: command must be a single line of printable characters (join steps with &&)", what)
		}
	}
	if strings.HasSuffix(cmd, `\`) {
		return fmt.Errorf("%s: command must not end with a backslash", what)
	}
	return nil
}

// Validate checks a set read from a file (or decoded from storage).
func (s Set) Validate() error {
	if len(s.Workers) > MaxWorkers {
		return fmt.Errorf("at most %d worker processes are allowed, got %d", MaxWorkers, len(s.Workers))
	}
	if len(s.Crons) > MaxCrons {
		return fmt.Errorf("at most %d cron jobs are allowed, got %d", MaxCrons, len(s.Crons))
	}
	if s.NoWeb && len(s.Workers) == 0 && len(s.Crons) == 0 {
		return fmt.Errorf("web is none but there is no worker or cron: the deployment would run nothing")
	}
	if s.WebCommand != "" {
		if err := checkCommand("process web", s.WebCommand); err != nil {
			return err
		}
	}
	seen := map[string]string{}
	taken := func(kind, name string) error {
		if name == Web {
			return fmt.Errorf("%s name %q is reserved for the HTTP process", kind, name)
		}
		if other, ok := seen[name]; ok {
			return fmt.Errorf("%s %q: the name is already used by a %s (names are shared by processes and crons)", kind, name, other)
		}
		seen[name] = kind
		return nil
	}
	for _, w := range s.Workers {
		if err := checkName("process", w.Name); err != nil {
			return err
		}
		if err := taken("process", w.Name); err != nil {
			return err
		}
		if err := checkCommand("process "+w.Name, w.Command); err != nil {
			return err
		}
		if w.Replicas < 0 || w.Replicas > MaxReplicas {
			return fmt.Errorf("process %s: replicas must be between 0 and %d, got %d", w.Name, MaxReplicas, w.Replicas)
		}
	}
	for _, c := range s.Crons {
		if err := checkName("cron", c.Name); err != nil {
			return err
		}
		if err := taken("cron", c.Name); err != nil {
			return err
		}
		if err := checkCommand("cron "+c.Name, c.Command); err != nil {
			return err
		}
		if err := ValidSchedule(c.Schedule); err != nil {
			return fmt.Errorf("cron %s: %w", c.Name, err)
		}
	}
	return nil
}

// ---- which processes run where ----

// Role is how an app's aliases use a deployment.
type Role int

const (
	// RoleNone: no alias points at the deployment; nothing runs.
	RoleNone Role = iota
	// RolePreview: a branch preview alias points at it; processes with
	// previews: true run.
	RolePreview
	// RoleProduction: the production alias points at it; everything runs.
	RoleProduction
)

// Activity is what of one deployment's processes should run: worker name →
// replicas, cron name → active (not suspended).
type Activity struct {
	Workers map[string]int32
	Crons   map[string]bool
}

// Activity returns what runs for a deployment in role. overrides are the
// app's replica overrides by worker name; they apply to production only
// (previews use the file's replicas).
func (s Set) Activity(role Role, overrides map[string]int) Activity {
	a := Activity{Workers: map[string]int32{}, Crons: map[string]bool{}}
	for _, w := range s.Workers {
		n := 0
		switch {
		case role == RoleProduction:
			n = w.Replicas
			if o, ok := overrides[w.Name]; ok {
				n = o
			}
		case role == RolePreview && w.Previews:
			n = w.Replicas
		}
		a.Workers[w.Name] = int32(n)
	}
	for _, c := range s.Crons {
		a.Crons[c.Name] = role == RoleProduction || (role == RolePreview && c.Previews)
	}
	return a
}

// Deployment is one deployment of an app as the reconcile sees it.
type Deployment struct {
	ID int64
	// InFlight: queued, building or deploying. Its processes are left
	// alone: the deploy starts and checks its workers itself.
	InFlight bool
	// Ready: finished successfully and not retired.
	Ready      bool
	Production bool // the production alias points at it
	// Preview: a branch preview alias points at it and it runs in the
	// preview environment.
	Preview bool
	Set     Set
}

// Role is the deployment's role for its processes.
func (d Deployment) Role() Role {
	switch {
	case !d.Ready:
		return RoleNone
	case d.Production:
		return RoleProduction
	case d.Preview:
		return RolePreview
	}
	return RoleNone
}

// Plan is the desired state of an app's processes by deployment id. A nil
// entry means "leave as is" (in flight); deployments without an entry stop
// all their processes.
type Plan map[int64]*Activity

// PlanFor applies the one-generation rule to every deployment of an app.
func PlanFor(deps []Deployment, overrides map[string]int) Plan {
	p := Plan{}
	for _, d := range deps {
		if d.InFlight {
			p[d.ID] = nil
			continue
		}
		a := d.Set.Activity(d.Role(), overrides)
		p[d.ID] = &a
	}
	return p
}

// Leave reports whether the processes of deployment id must not be touched.
func (p Plan) Leave(id int64) bool {
	a, ok := p[id]
	return ok && a == nil
}

// Replicas is the desired replica count of a worker (0 when unknown).
func (p Plan) Replicas(id int64, worker string) int32 {
	if a := p[id]; a != nil {
		return a.Workers[worker]
	}
	return 0
}

// CronActive reports whether a cron job should be scheduled.
func (p Plan) CronActive(id int64, cron string) bool {
	if a := p[id]; a != nil {
		return a.Crons[cron]
	}
	return false
}

// ---- runtime status (API, UI) ----

// Status is the cluster state of a deployment's processes.
type Status struct {
	Web     *ReplicaStatus           `json:"web,omitempty"`
	Workers map[string]ReplicaStatus `json:"workers"`
	Crons   map[string]CronStatus    `json:"crons"`
}

// ReplicaStatus is the state of a web or worker Deployment.
type ReplicaStatus struct {
	Desired int32 `json:"desired"`
	Ready   int32 `json:"ready"`
	// State: "running", "starting", "stopped", "sleeping", "crashing" or
	// "missing" (no object in the cluster).
	State string `json:"state"`
	// Reason of a failing pod, e.g. "CrashLoopBackOff".
	Reason string `json:"reason,omitempty"`
}

// CronStatus is the state of a CronJob and its last run.
type CronStatus struct {
	Exists             bool       `json:"exists"`
	Suspended          bool       `json:"suspended"`
	Running            int        `json:"running"`
	LastScheduleTime   *time.Time `json:"last_schedule_time,omitempty"`
	LastSuccessfulTime *time.Time `json:"last_successful_time,omitempty"`
	LastJob            *JobStatus `json:"last_job,omitempty"`
}

// JobStatus is one run of a cron job.
type JobStatus struct {
	Name string `json:"name"`
	// Status: "running", "succeeded" or "failed".
	Status   string     `json:"status"`
	Manual   bool       `json:"manual,omitempty"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
}
