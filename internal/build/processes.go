package build

import (
	"fmt"
	"strings"

	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/worker"
)

// Faz 20: process types. The build reads the process configuration of the
// commit it builds (process.Load: paas.yaml, else the Procfile) and records
// it with the deployment, so the processes of a deployment always match its
// image.
//
// Web command precedence: the start command setting, then processes.web in
// paas.yaml, then the Procfile's web line, then what detection finds. With
// a paas.yaml the Procfile is ignored entirely.
//
// Apps without a web process: "web: none" in paas.yaml, or, when the file
// does not mention web, a project whose start command cannot be detected
// (e.g. a Python bot without a web framework) but that has workers. The
// image is then built with the first worker's command as its start command
// (any command would do: every process runs with its own), and the
// deployment gets no Deployment, Service or Ingress for web.

// detectWithProcesses loads the process configuration of dir, detects the
// project with it and returns the plan and the resolved process set.
func detectWithProcesses(dir string, o Options, log worker.Logger) (Plan, process.Set, error) {
	cfg, err := process.Load(dir)
	if err != nil {
		return Plan{}, process.Set{}, fmt.Errorf("process configuration: %w", err)
	}
	set := cfg.Set
	for _, n := range cfg.Notes {
		log("    %s", n)
	}
	fromFile := set.Source != process.SourceDefault && set.Source != process.SourceProcfile
	if fromFile {
		o.noProcfile = true
	}
	switch {
	case o.StartCommand != "" && set.WebCommand != "" && fromFile:
		log("    the start command setting replaces processes.web of %s", set.Source)
	case o.StartCommand != "":
	case set.NoWeb:
		o.StartCommand = firstCommand(set)
	case fromFile && set.WebCommand != "" && len(set.WebExec) == 0:
		o.StartCommand = set.WebCommand
	}

	plan, err := DetectWith(dir, o)
	if err != nil && !cfg.WebDeclared && len(set.Workers) > 0 && o.StartCommand == "" {
		// No HTTP server to be found, but there are workers: a bot or a
		// queue consumer. Build the image around the first worker.
		o.StartCommand = firstCommand(set)
		if p, err2 := DetectWith(dir, o); err2 == nil {
			log("    no web start command detected (%v); deploying without a web process "+
				"(write \"web: none\" in paas.yaml to make this explicit)", err)
			plan, err, set.NoWeb = p, nil, true
		}
	}
	if err != nil {
		return Plan{}, process.Set{}, err
	}

	// Go images are distroless: no shell, commands run as split words.
	set.Resolve(plan.Kind != KindGo)
	if set.HasWeb() && fromFile && plan.Kind == KindDockerfile && set.WebCommand != "" && len(set.WebExec) == 0 {
		// A repository Dockerfile keeps its CMD; the command replaces it at run time.
		set.WebExec = []string{"sh", "-c", set.WebCommand}
	}
	if !set.HasWeb() {
		set.WebCommand, set.WebExec = "", nil
	}
	if !set.Empty() || set.Source != process.SourceDefault {
		log("==> processes (%s): %s", sourceName(set), describe(set))
	}
	return plan, set, nil
}

// firstCommand is the command an image without a web process is built
// around: the first worker's, else the first cron's.
func firstCommand(s process.Set) string {
	if len(s.Workers) > 0 {
		return s.Workers[0].Command
	}
	if len(s.Crons) > 0 {
		return s.Crons[0].Command
	}
	return ""
}

func sourceName(s process.Set) string {
	if s.Source == process.SourceDefault {
		return "default"
	}
	return s.Source
}

// describe is the one-line summary of a set for the build log, e.g.
// "web, worker ×2 (previews), bot; cron cleanup (*/15 * * * *)".
func describe(s process.Set) string {
	var procs []string
	if s.HasWeb() {
		procs = append(procs, "web")
	} else {
		procs = append(procs, "no web")
	}
	for _, w := range s.Workers {
		p := w.Name
		if w.Replicas != 1 {
			p += fmt.Sprintf(" ×%d", w.Replicas)
		}
		if w.Previews {
			p += " (previews)"
		}
		procs = append(procs, p)
	}
	out := strings.Join(procs, ", ")
	for i, c := range s.Crons {
		if i == 0 {
			out += "; cron "
		} else {
			out += ", "
		}
		out += c.Name + " (" + c.Schedule + ")"
	}
	return out
}
