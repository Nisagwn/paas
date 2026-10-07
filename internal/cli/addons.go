package cli

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Faz 22: managed databases.
//
//	paas addons ls <app>
//	paas addons add <app> postgres|redis [--name n] [--plan p] [--preview-mode copy|empty|shared]
//	paas addons info <app> <addon> [--reveal [--branch b]]
//	paas addons set <app> <addon> [--plan p] [--preview-mode m] [--anonymize RULE]... [--anonymize-sql-file F] [--backup-keep N]
//	paas addons rm <app> <addon> --confirm <addon>
//	paas addons rotate <app> <addon> [--no-redeploy]
//	paas addons branches <app> <addon>
//	paas addons reset <app> <addon> <branch>
//	paas addons backups|backup <app> <addon>
//	paas addons restore <app> <addon> <backup-id> --confirm <addon>
//	paas db psql <app> [addon] [--branch b] [--port 15432]

type addonPlan struct {
	ID      string `json:"id"`
	Memory  string `json:"memory"`
	Storage string `json:"storage"`
}

type addonInfo struct {
	ID           int64     `json:"id"`
	Kind         string    `json:"kind"`
	Name         string    `json:"name"`
	Plan         string    `json:"plan"`
	Status       string    `json:"status"`
	Message      string    `json:"message"`
	PreviewMode  string    `json:"preview_mode"`
	Anonymize    []string  `json:"anonymize"`
	AnonymizeSQL string    `json:"anonymize_sql"`
	BackupKeep   int       `json:"backup_keep"`
	Rotating     bool      `json:"rotating"`
	CreatedAt    time.Time `json:"created_at"`
	Size         addonPlan `json:"size"`
	Host         string    `json:"host"`
	Port         int       `json:"port"`
	User         string    `json:"user"`
	Database     string    `json:"database"`
	Variables    []string  `json:"variables"`
}

type addonCreds struct {
	Addon     string            `json:"addon"`
	Kind      string            `json:"kind"`
	Branch    string            `json:"branch"`
	Host      string            `json:"host"`
	Port      int               `json:"port"`
	User      string            `json:"user"`
	Password  string            `json:"password"`
	Database  string            `json:"database"`
	URL       string            `json:"url"`
	Variables map[string]string `json:"variables"`
}

type addonBranch struct {
	Branch     string     `json:"branch"`
	Database   string     `json:"database"`
	Mode       string     `json:"mode"`
	Status     string     `json:"status"`
	Error      string     `json:"error"`
	Warning    string     `json:"warning"`
	SnapshotAt *time.Time `json:"snapshot_at"`
	SizeBytes  *int64     `json:"size_bytes"`
}

type addonBackup struct {
	ID            int64      `json:"id"`
	Job           string     `json:"job"`
	Trigger       string     `json:"trigger"`
	Status        string     `json:"status"`
	Error         string     `json:"error"`
	SizeBytes     *int64     `json:"size_bytes"`
	StartedAt     *time.Time `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	RestoreStatus string     `json:"restore_status"`
	RestoreError  string     `json:"restore_error"`
	CreatedAt     time.Time  `json:"created_at"`
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ", ") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

type addonFlags struct {
	name, plan, previewMode, confirm, branch, sqlFile string
	backupKeep                                        int
	reveal, noRedeploy, clearRules                    bool
	anonymize                                         stringList
}

func (f *addonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.name, "name", "", "add: add-on `name` (default db for postgres, cache for redis)")
	fs.StringVar(&f.plan, "plan", "", "add, set: `size` hobby, standard or pro")
	fs.StringVar(&f.previewMode, "preview-mode", "", "add, set: what previews connect to: copy, empty or shared (Postgres)")
	fs.StringVar(&f.confirm, "confirm", "", "rm, restore: the add-on's name, to confirm")
	fs.StringVar(&f.branch, "branch", "", "info --reveal: a branch database instead of production")
	fs.StringVar(&f.sqlFile, "anonymize-sql-file", "", "set: `file` with SQL statements run in every copy after the rules (\"-\" clears)")
	fs.IntVar(&f.backupKeep, "backup-keep", -1, "set: daily backups kept, 0-30 (0: no daily backups)")
	fs.BoolVar(&f.reveal, "reveal", false, "info: also print the password and connection URL (logged on the server)")
	fs.BoolVar(&f.noRedeploy, "no-redeploy", false, "rotate: do not redeploy after the new password is applied")
	fs.BoolVar(&f.clearRules, "clear-anonymize", false, "set: remove every anonymization rule")
	fs.Var(&f.anonymize, "anonymize", "set: anonymization `rule` like \"users.email: email\" (repeatable; replaces the rules)")
}

func addonPath(app, addon string, rest ...string) string {
	return appPath(app, append([]string{"addons", url.PathEscape(addon)}, rest...)...)
}

func cmdAddons(r *runner, args []string) error {
	fs := r.flags()
	var f addonFlags
	f.register(fs)
	pos, err := r.parse(fs, args, 2, 4)
	if err != nil {
		return err
	}
	sub, appName, rest := pos[0], pos[1], pos[2:]
	want := map[string]int{"ls": 0, "list": 0, "add": 1, "info": 1, "set": 1, "rm": 1, "remove": 1, "rotate": 1,
		"branches": 1, "reset": 2, "backups": 1, "backup": 1, "restore": 2}
	n, ok := want[sub]
	if !ok {
		return usagef("unknown addons subcommand %q (want ls, add, info, set, rm, rotate, branches, reset, backups, backup or restore)", sub)
	}
	if len(rest) != n {
		return usagef("%s takes %d argument(s) after the app", sub, n)
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	switch sub {
	case "ls", "list":
		var list []addonInfo
		raw, err := c.Do(r.ctx, "GET", appPath(appName, "addons"), nil, &list)
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		if len(list) == 0 {
			fmt.Fprintf(r.Stdout, "%s has no add-ons. Add one with: paas addons add %s postgres\n", appName, appName)
			return nil
		}
		tw := newTable(r.Stdout)
		fmt.Fprintln(tw, "NAME\tKIND\tPLAN\tSTATUS\tPREVIEWS\tVARIABLES")
		for _, a := range list {
			status := a.Status
			if a.Rotating {
				status += " (rotating)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Kind, a.Plan, status, a.PreviewMode, strings.Join(a.Variables, ","))
		}
		return tw.Flush()
	case "add":
		body := map[string]string{"kind": rest[0], "name": f.name, "plan": f.plan, "preview_mode": f.previewMode}
		var a addonInfo
		raw, err := c.Do(r.ctx, "POST", appPath(appName, "addons"), body, &a)
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		fmt.Fprintf(r.Stdout, "Created %s add-on %s on %s (%s, %s memory, %s disk); it is being provisioned.\n",
			a.Kind, a.Name, appName, a.Size.ID, a.Size.Memory, a.Size.Storage)
		fmt.Fprintf(r.Stdout, "Deployments get %s once it is ready; redeploy running ones to pick them up.\n",
			strings.Join(a.Variables, ", "))
		fmt.Fprintf(r.Stdout, "Follow it with: paas addons info %s %s\n", appName, a.Name)
		return nil
	case "info":
		return r.addonInfo(c, appName, rest[0], f)
	case "set":
		return r.addonSet(c, appName, rest[0], f, fs)
	case "rm", "remove":
		if f.confirm != rest[0] {
			return usagef("deleting %s deletes its data, copies and backups: pass --confirm %s", rest[0], rest[0])
		}
		raw, err := c.Do(r.ctx, "DELETE", addonPath(appName, rest[0])+"?confirm="+url.QueryEscape(f.confirm), nil, nil)
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		fmt.Fprintf(r.Stdout, "Deleting %s of %s with its volumes. Deployments keep its variables until they are redeployed.\n", rest[0], appName)
		return nil
	case "rotate":
		var res struct {
			Note string `json:"note"`
		}
		raw, err := c.Do(r.ctx, "POST", addonPath(appName, rest[0], "rotate"), map[string]bool{"redeploy": !f.noRedeploy}, &res)
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		fmt.Fprintf(r.Stdout, "Rotating the password of %s: %s.\n", rest[0], res.Note)
		return nil
	case "branches":
		var list []addonBranch
		raw, err := c.Do(r.ctx, "GET", addonPath(appName, rest[0], "branches"), nil, &list)
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		if len(list) == 0 {
			fmt.Fprintf(r.Stdout, "%s has no branch databases yet: the first deployment of a preview branch creates one.\n", rest[0])
			return nil
		}
		now := r.Now()
		tw := newTable(r.Stdout)
		fmt.Fprintln(tw, "BRANCH\tDATABASE\tMODE\tSTATUS\tSNAPSHOT\tSIZE\tNOTE")
		for _, b := range list {
			snap := "-"
			if b.SnapshotAt != nil {
				snap = relTime(now, *b.SnapshotAt)
			}
			note := b.Error
			if note == "" {
				note = b.Warning
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", b.Branch, b.Database, b.Mode, b.Status, snap, sizeText(b.SizeBytes), oneLine(note, 80))
		}
		return tw.Flush()
	case "reset":
		raw, err := c.Do(r.ctx, "POST", addonPath(appName, rest[0], "branches", url.PathEscape(rest[1]), "reset"), nil, nil)
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		fmt.Fprintf(r.Stdout, "Copying production into %s's database of %s again. Follow it with: paas addons branches %s %s\n",
			rest[1], rest[0], appName, rest[0])
		return nil
	case "backups":
		var list []addonBackup
		raw, err := c.Do(r.ctx, "GET", addonPath(appName, rest[0], "backups"), nil, &list)
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		if len(list) == 0 {
			fmt.Fprintf(r.Stdout, "%s has no backups yet. Take one with: paas addons backup %s %s\n", rest[0], appName, rest[0])
			return nil
		}
		now := r.Now()
		tw := newTable(r.Stdout)
		fmt.Fprintln(tw, "ID\tTAKEN\tTRIGGER\tSTATUS\tSIZE\tRESTORE")
		for _, b := range list {
			taken := relTime(now, b.CreatedAt)
			if b.FinishedAt != nil {
				taken = relTime(now, *b.FinishedAt)
			}
			status := b.Status
			if b.Error != "" {
				status += ": " + oneLine(b.Error, 60)
			}
			restore := orDash(b.RestoreStatus)
			if b.RestoreError != "" {
				restore += ": " + oneLine(b.RestoreError, 60)
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", b.ID, taken, b.Trigger, status, sizeText(b.SizeBytes), restore)
		}
		return tw.Flush()
	case "backup":
		var b addonBackup
		raw, err := c.Do(r.ctx, "POST", addonPath(appName, rest[0], "backups"), nil, &b)
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		fmt.Fprintf(r.Stdout, "Backup #%d of %s started. Follow it with: paas addons backups %s %s\n", b.ID, rest[0], appName, rest[0])
		return nil
	case "restore":
		id, err := strconv.ParseInt(strings.TrimPrefix(rest[1], "#"), 10, 64)
		if err != nil || id <= 0 {
			return usagef("invalid backup id %q", rest[1])
		}
		if f.confirm != rest[0] {
			return usagef("restoring replaces the production database of %s: pass --confirm %s", rest[0], rest[0])
		}
		raw, err := c.Do(r.ctx, "POST", addonPath(appName, rest[0], "backups", strconv.FormatInt(id, 10), "restore"),
			map[string]string{"confirm": f.confirm}, nil)
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		fmt.Fprintf(r.Stdout, "Restoring backup #%d into the production database of %s (one transaction: on error nothing changes).\n"+
			"Follow it with: paas addons backups %s %s\n", id, rest[0], appName, rest[0])
		return nil
	}
	return nil
}

func sizeText(n *int64) string {
	if n == nil {
		return "-"
	}
	const unit = 1024
	v := float64(*n)
	for _, u := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if v < unit || u == "TiB" {
			if u == "B" {
				return fmt.Sprintf("%d B", *n)
			}
			return fmt.Sprintf("%.1f %s", v, u)
		}
		v /= unit
	}
	return ""
}

func (r *runner) addonInfo(c *Client, app, addon string, f addonFlags) error {
	var a addonInfo
	raw, err := c.Do(r.ctx, "GET", addonPath(app, addon), nil, &a)
	if err != nil {
		return err
	}
	var creds *addonCreds
	if f.reveal {
		creds = &addonCreds{}
		body := map[string]string{}
		if f.branch != "" {
			body["branch"] = f.branch
		}
		if _, err := c.Do(r.ctx, "POST", addonPath(app, addon, "credentials", "reveal"), body, creds); err != nil {
			return err
		}
	} else if f.branch != "" {
		return usagef("--branch needs --reveal")
	}
	if r.g.JSON {
		if creds != nil {
			return writeJSON(r.Stdout, map[string]any{"addon": a, "credentials": creds})
		}
		return writeRawJSON(r.Stdout, raw)
	}
	tw := newTable(r.Stdout)
	status := a.Status
	if a.Message != "" {
		status += " (" + a.Message + ")"
	}
	rows := [][2]string{
		{"Add-on", a.Name + " (" + a.Kind + ")"},
		{"Status", status},
		{"Plan", fmt.Sprintf("%s: %s memory, %s disk", a.Size.ID, a.Size.Memory, a.Size.Storage)},
		{"Host", fmt.Sprintf("%s:%d", a.Host, a.Port)},
	}
	if a.User != "" {
		rows = append(rows, [2]string{"User / database", a.User + " / " + a.Database})
	}
	rows = append(rows, [2]string{"Previews", a.PreviewMode}, [2]string{"Variables", strings.Join(a.Variables, ", ")})
	if a.Kind == "postgres" {
		rules := "none"
		if len(a.Anonymize) > 0 {
			rules = strings.Join(a.Anonymize, "; ")
		}
		if a.AnonymizeSQL != "" {
			rules += " (+ SQL statements)"
		}
		rows = append(rows, [2]string{"Anonymization", rules}, [2]string{"Daily backups kept", strconv.Itoa(a.BackupKeep)})
	}
	if a.Rotating {
		rows = append(rows, [2]string{"Rotation", "a new password is being applied"})
	}
	for _, row := range rows {
		fmt.Fprintf(tw, "%s:\t%s\n", row[0], row[1])
	}
	if creds != nil {
		where := "production"
		if creds.Branch != "" {
			where = "branch " + creds.Branch
		}
		fmt.Fprintf(tw, "Password (%s):\t%s\n", where, creds.Password)
		fmt.Fprintf(tw, "URL:\t%s\n", creds.URL)
	}
	tw.Flush()
	if creds != nil {
		fmt.Fprintln(r.Stderr, "(this reveal was logged on the server)")
	}
	return nil
}

func (r *runner) addonSet(c *Client, app, addon string, f addonFlags, fs *flag.FlagSet) error {
	body := map[string]any{}
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if set["plan"] {
		body["plan"] = f.plan
	}
	if set["preview-mode"] {
		body["preview_mode"] = f.previewMode
	}
	switch {
	case f.clearRules && len(f.anonymize) > 0:
		return usagef("--clear-anonymize and --anonymize exclude each other")
	case f.clearRules:
		body["anonymize"] = []string{}
	case len(f.anonymize) > 0:
		body["anonymize"] = []string(f.anonymize)
	}
	switch f.sqlFile {
	case "":
	case "-":
		body["anonymize_sql"] = ""
	default:
		b, err := os.ReadFile(f.sqlFile)
		if err != nil {
			return err
		}
		body["anonymize_sql"] = string(b)
	}
	if set["backup-keep"] {
		body["backup_keep"] = f.backupKeep
	}
	if len(body) == 0 {
		return usagef("nothing to change: give --plan, --preview-mode, --anonymize, --clear-anonymize, --anonymize-sql-file or --backup-keep")
	}
	var a addonInfo
	raw, err := c.Do(r.ctx, "PATCH", addonPath(app, addon), body, &a)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	fmt.Fprintf(r.Stdout, "Updated %s of %s (plan %s, previews %s, %d anonymization rule(s), %d daily backup(s) kept).\n",
		a.Name, app, a.Plan, a.PreviewMode, len(a.Anonymize), a.BackupKeep)
	return nil
}

// cmdDB: `paas db psql` prints how to open a SQL session from a laptop.
// The platform has no tunnel of its own: the add-ons are reachable only
// inside the app's namespace (NetworkPolicy), so the command shows the
// kubectl port-forward that gets there for anyone with cluster access,
// and the psql command with the revealed credentials (logged).
func cmdDB(r *runner, args []string) error {
	fs := r.flags()
	branch := fs.String("branch", "", "connect to this branch's database instead of production")
	port := fs.Int("port", 15432, "local `port` of the tunnel")
	pos, err := r.parse(fs, args, 2, 3)
	if err != nil {
		return err
	}
	if pos[0] != "psql" {
		return usagef("unknown db subcommand %q (want psql)", pos[0])
	}
	appName := pos[1]
	c, err := r.client()
	if err != nil {
		return err
	}
	addon := ""
	if len(pos) == 3 {
		addon = pos[2]
	} else {
		var list []addonInfo
		if _, err := c.Do(r.ctx, "GET", appPath(appName, "addons"), nil, &list); err != nil {
			return err
		}
		for _, a := range list {
			if a.Kind == "postgres" && (addon == "" || a.Name == "db") {
				addon = a.Name
			}
		}
		if addon == "" {
			return &hintError{msg: appName + " has no Postgres add-on", hint: "Add one with: paas addons add " + appName + " postgres"}
		}
	}
	var creds addonCreds
	body := map[string]string{}
	if *branch != "" {
		body["branch"] = *branch
	}
	if _, err := c.Do(r.ctx, "POST", addonPath(appName, addon, "credentials", "reveal"), body, &creds); err != nil {
		return err
	}
	if creds.Kind != "postgres" {
		return usagef("%s is a %s add-on; psql needs a Postgres one", addon, creds.Kind)
	}
	local := (&url.URL{Scheme: "postgres", User: url.UserPassword(creds.User, creds.Password),
		Host: fmt.Sprintf("localhost:%d", *port), Path: "/" + creds.Database, RawQuery: "sslmode=disable"}).String()
	if r.g.JSON {
		return writeJSON(r.Stdout, map[string]any{
			"namespace": "app-" + appName, "service": "addon-" + addon, "local_port": *port, "url": local,
			"port_forward": portForward(appName, addon, *port, creds.Port),
		})
	}
	where := "production"
	if *branch != "" {
		where = "the database of branch " + *branch
	}
	fmt.Fprintf(r.Stdout, "%s of %s (%s). The add-on is reachable only inside the cluster, so open a tunnel first\n"+
		"(needs kubectl access to the cluster; the platform has no tunnel of its own):\n\n  %s\n\nthen, in another terminal:\n\n  psql %q\n\n",
		addon, appName, where, portForward(appName, addon, *port, creds.Port), local)
	fmt.Fprintln(r.Stderr, "(the credentials were revealed for this command; the server logged it)")
	return nil
}

func portForward(app, addon string, local, remote int) string {
	return fmt.Sprintf("kubectl -n app-%s port-forward svc/addon-%s %d:%d", app, addon, local, remote)
}
