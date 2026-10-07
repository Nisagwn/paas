// Package cli implements the `paas` command-line client (Faz 18): login
// with a personal API token, apps, deployments, build and runtime logs,
// rollback, environment variables, custom domains and GitHub imports.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// App holds the process environment, replaceable in tests.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	Now    func() time.Time
	HTTP   *http.Client
	// OpenURL opens a URL in the browser (`paas open`).
	OpenURL func(string) error
	// ReconnectDelay is the pause before a dropped log stream reconnects.
	ReconnectDelay time.Duration
}

// New returns an App wired to the real process.
func New() *App {
	return &App{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getenv: os.Getenv, Now: time.Now, HTTP: &http.Client{},
		OpenURL: openBrowser, ReconnectDelay: time.Second,
	}
}

// globals are the flags every command accepts.
type globals struct {
	URL   string
	Token string
	JSON  bool
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.URL, "url", g.URL, "platform `URL` (default: PAAS_URL or the URL saved by `paas login`)")
	fs.StringVar(&g.Token, "token", g.Token, "personal API `token` (default: PAAS_TOKEN or the saved token)")
	fs.BoolVar(&g.JSON, "json", g.JSON, "print machine-readable JSON")
}

type command struct {
	name    string
	args    string
	summary string
	// help is extra text under the summary in `paas help <command>`.
	help string
	run  func(r *runner, args []string) error
}

func commandList() []command {
	return []command{
		{name: "login", args: "[--url https://host] [--token paas_…]", summary: "Log in with a personal API token",
			help: "Create a token on the Tokens page of the web UI (/tokens). The token is verified with\n" +
				"GET /api/me and stored with the URL in the user config directory (mode 0600).",
			run: cmdLogin},
		{name: "logout", summary: "Remove the stored URL and token", run: cmdLogout},
		{name: "whoami", summary: "Show the logged-in user and team roles", run: cmdWhoami},
		{name: "ls", summary: "List apps with production URL and last deployment", run: cmdLs},
		{name: "deployments", args: "<app> [--limit N]", summary: "List an app's deployments", run: cmdDeployments},
		{name: "inspect", args: "<deployment-id>", summary: "Show one deployment", run: cmdInspect},
		{name: "logs", args: "<deployment-id> [-f] | --runtime [--process NAME] <app> <deployment-id> [-f] [--tail N]",
			summary: "Print build logs (-f follows until the deployment finishes) or pod logs (--runtime)",
			help:    "With -f the command exits 0 when the deployment is ready and 1 when it failed.",
			run:     cmdLogs},
		{name: "rollback", args: "<app> <deployment-id>", summary: "Point production at an earlier deployment", run: cmdRollback},
		{name: "env", args: "ls <app> | set <app> KEY=VALUE... | rm <app> KEY...",
			summary: "Manage environment variables (values are write-only)",
			help:    "Changes apply to deployments created afterwards.", run: cmdEnv},
		{name: "domains", args: "ls <app> | add <app> <host> | verify <app> <host> | rm <app> <host>",
			summary: "Manage custom domains", run: cmdDomains},
		{name: "import", args: "<owner/repo> [--name n] [--team t] [--branch b]",
			summary: "Import a GitHub repository and queue its first deployment", run: cmdImport},
		{name: "open", args: "<app> [--print]", summary: "Open the app's production URL in the browser", run: cmdOpen},
		// Faz 20 (processes.go).
		{name: "ps", args: "<app> [--deployment ID] | scale <app> NAME=N...",
			summary: "Show an app's processes (web, workers, crons) or scale its workers",
			help: "Processes come from paas.yaml (or the Procfile) of the deployed commit. Workers and crons run\n" +
				"for the production deployment only. scale sets an app-wide override for production;\n" +
				"NAME=default goes back to the replicas in paas.yaml.",
			run: cmdPs},
		{name: "cron", args: "run <app> <name>", summary: "Run a cron job of the production deployment now", run: cmdCron},
		// Faz 21 (rollout.go).
		{name: "rollout", args: "status|promote|abort|pause|resume|rollback <app> | settings <app> [--mode canary] [--steps 10,50,100] [--step 5m] ...",
			summary: "Show or steer canary / guarded rollouts and their settings",
			help: "Modes: instant (default), guarded (switch at once, watch, roll back on regression),\n" +
				"canary (the new production takes --steps of the traffic, one --step at a time).\n" +
				"Without flags `settings` prints the current settings.",
			run: cmdRollout},
		// Faz 22 (addons.go).
		{name: "addons", args: "ls|add|info|set|rm|rotate|branches|reset|backups|backup|restore <app> [addon] ...",
			summary: "Manage managed databases (Postgres, Redis) and their preview copies and backups",
			help: "  ls <app>                                    list add-ons\n" +
				"  add <app> postgres|redis [--name n] [--plan hobby|standard|pro] [--preview-mode copy|empty|shared]\n" +
				"  info <app> <addon> [--reveal [--branch b]] details; --reveal prints the password (logged)\n" +
				"  set <app> <addon> [--plan p] [--preview-mode m] [--anonymize \"users.email: email\"]...\n" +
				"      [--clear-anonymize] [--anonymize-sql-file f.sql|-] [--backup-keep N]\n" +
				"  rm <app> <addon> --confirm <addon>        delete with data, copies and backups\n" +
				"  rotate <app> <addon> [--no-redeploy]      new password; then redeploys what the aliases point at\n" +
				"  branches <app> <addon>                    preview branch databases\n" +
				"  reset <app> <addon> <branch>              copy production into the branch's database again\n" +
				"  backups <app> <addon> | backup <app> <addon>\n" +
				"  restore <app> <addon> <backup-id> --confirm <addon>",
			run: cmdAddons},
		{name: "db", args: "psql <app> [addon] [--branch b] [--port 15432]",
			summary: "Print the kubectl port-forward and psql commands for an add-on database",
			help: "The add-ons are reachable only inside the cluster. The command reveals the credentials (logged)\n" +
				"and prints a port-forward for anyone with kubectl access, then the psql command for it.",
			run: cmdDB},
	}
}

func findCommand(name string) (command, bool) {
	for _, c := range commandList() {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

func (a *App) usage(w io.Writer) {
	fmt.Fprint(w, "paas: command-line client for the paas platform\n\nUsage:\n  paas [--url URL] [--token TOKEN] [--json] <command> [arguments]\n\nCommands:\n")
	tw := newTable(w)
	for _, c := range commandList() {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	fmt.Fprintf(tw, "  %s\t%s\n", "help", "Show help for a command")
	tw.Flush()
	fmt.Fprint(w, "\nGlobal flags (before or after the command):\n"+
		"  --url URL      platform URL (PAAS_URL)\n"+
		"  --token TOKEN  personal API token (PAAS_TOKEN)\n"+
		"  --json         machine-readable output\n\n"+
		"Run 'paas help <command>' for details. Exit codes: 0 ok, 1 error, 2 usage.\n")
}

// usageError is a wrong invocation (exit 2). printed: flag already told.
type usageError struct {
	msg     string
	printed bool
}

func (e *usageError) Error() string { return e.msg }

// Run executes one command line and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	g := &globals{}
	fs := flag.NewFlagSet("paas", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	g.register(fs)
	fs.Usage = func() { a.usage(a.Stderr) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		a.usage(a.Stderr)
		return ExitUsage
	}
	name, rest := rest[0], rest[1:]
	if name == "help" {
		if len(rest) == 0 {
			a.usage(a.Stdout)
			return ExitOK
		}
		name, rest = rest[0], []string{"-h"}
	}
	cmd, ok := findCommand(name)
	if !ok {
		fmt.Fprintf(a.Stderr, "paas: unknown command %q\nRun 'paas help' for usage.\n", name)
		return ExitUsage
	}
	r := &runner{App: a, ctx: ctx, g: g, cmd: cmd}
	return r.report(cmd.run(r, rest))
}

// runner is the state of one command invocation.
type runner struct {
	*App
	ctx context.Context
	g   *globals
	cmd command
}

func (r *runner) report(err error) int {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		if !ue.printed {
			fmt.Fprintf(r.Stderr, "paas %s: %s\nUsage: paas %s %s\n", r.cmd.name, ue.msg, r.cmd.name, r.cmd.args)
		}
		return ExitUsage
	}
	if errors.Is(err, context.Canceled) && r.ctx.Err() != nil {
		return ExitError // interrupted (Ctrl+C)
	}
	fmt.Fprintln(r.Stderr, "Error:", err)
	var h interface{ Hint() string }
	if errors.As(err, &h) && h.Hint() != "" {
		fmt.Fprintln(r.Stderr, "Hint:", h.Hint())
	}
	return ExitError
}

func usagef(format string, a ...any) error { return &usageError{msg: fmt.Sprintf(format, a...)} }

// flags is a FlagSet for the current command with the global flags.
func (r *runner) flags() *flag.FlagSet {
	fs := flag.NewFlagSet("paas "+r.cmd.name, flag.ContinueOnError)
	fs.SetOutput(r.Stderr)
	r.g.register(fs)
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintf(w, "Usage: paas %s %s\n\n%s.\n", r.cmd.name, r.cmd.args, r.cmd.summary)
		if r.cmd.help != "" {
			fmt.Fprintf(w, "\n%s\n", r.cmd.help)
		}
		fmt.Fprintln(w, "\nFlags:")
		fs.PrintDefaults()
	}
	return fs
}

// parse parses flags anywhere among the arguments (`paas ls --json` and
// `paas --json ls`), stops at "--", and checks the positional count
// (max < 0: unlimited).
func (r *runner) parse(fs *flag.FlagSet, args []string, min, max int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &usageError{msg: err.Error(), printed: true}
		}
		rest := fs.Args()
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			pos = append(pos, rest...)
			break
		}
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	switch {
	case len(pos) < min:
		return nil, usagef("missing arguments")
	case max >= 0 && len(pos) > max:
		return nil, usagef("too many arguments: %s", strings.Join(pos[max:], " "))
	}
	return pos, nil
}

// client resolves URL and token: flag, then environment, then config. The
// stored token is only sent to the URL it was stored for.
func (r *runner) client() (*Client, error) {
	cfg, _, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	raw := first(r.g.URL, r.Getenv("PAAS_URL"), cfg.URL)
	if raw == "" {
		return nil, notLoggedIn()
	}
	base, err := NormalizeURL(raw)
	if err != nil {
		return nil, err
	}
	token := first(r.g.Token, r.Getenv("PAAS_TOKEN"))
	if token == "" && cfg.Token != "" {
		if stored, err := NormalizeURL(cfg.URL); err == nil && stored == base {
			token = cfg.Token
		}
	}
	if token == "" {
		return nil, notLoggedIn()
	}
	return &Client{BaseURL: base, Token: token, HTTP: r.HTTP}, nil
}

func notLoggedIn() error {
	return &hintError{msg: "not logged in",
		hint: "Run `paas login --url https://<platform>` or set PAAS_URL and PAAS_TOKEN."}
}

func first(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
