package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- API shapes (internal/api views; only the fields the CLI shows) ----

type team struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type me struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
	Teams []team `json:"teams"`
}

type alias struct {
	Hostname     string `json:"hostname"`
	Kind         string `json:"kind"`
	Branch       string `json:"branch"`
	DeploymentID int64  `json:"deployment_id"`
}

type app struct {
	Name             string    `json:"name"`
	Repo             string    `json:"repo"`
	ProductionBranch string    `json:"production_branch"`
	ProductionURL    string    `json:"production_url"`
	CreatedAt        time.Time `json:"created_at"`
	Aliases          []alias   `json:"aliases"`
}

type deployment struct {
	ID            int64      `json:"id"`
	AppName       string     `json:"app_name"`
	Repo          string     `json:"repo"`
	CommitSHA     string     `json:"commit_sha"`
	Branch        string     `json:"branch"`
	CommitMessage string     `json:"commit_message"`
	Status        string     `json:"status"`
	Error         string     `json:"error"`
	Image         string     `json:"image"`
	URL           string     `json:"url"`
	Sleeping      bool       `json:"sleeping"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	RetiredAt     *time.Time `json:"retired_at"`
	RetireReason  string     `json:"retire_reason"`
	Attempts      int        `json:"attempts"`
}

func (d deployment) statusText() string {
	switch {
	case d.Sleeping:
		return d.Status + " (sleeping)"
	case d.RetiredAt != nil && d.Status != "retired":
		return d.Status + " (retired)"
	}
	return d.Status
}

func (d deployment) buildTime() string {
	if d.StartedAt == nil || d.FinishedAt == nil {
		return "-"
	}
	return duration(d.FinishedAt.Sub(*d.StartedAt))
}

type logLine struct {
	ID   int64     `json:"id"`
	TS   time.Time `json:"ts"`
	Line string    `json:"line"`
}

type dnsRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
	Note  string `json:"note"`
}

type domain struct {
	Hostname   string      `json:"hostname"`
	Status     string      `json:"status"`
	Serving    bool        `json:"serving"`
	Error      string      `json:"error"`
	URL        string      `json:"url"`
	DNSRecords []dnsRecord `json:"dns_records"`
}

func appPath(name string, rest ...string) string {
	p := "/api/apps/" + url.PathEscape(name)
	for _, s := range rest {
		p += "/" + s
	}
	return p
}

func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, usagef("invalid deployment id %q: want a positive number", s)
	}
	return id, nil
}

// ---- login / logout / whoami ----

func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && s != "") {
		if errors.Is(err, io.EOF) {
			return "", errors.New("no input: pass --token or set PAAS_TOKEN when stdin is not a terminal")
		}
		return "", err
	}
	return strings.TrimSpace(s), nil
}

func cmdLogin(r *runner, args []string) error {
	fs := r.flags()
	if _, err := r.parse(fs, args, 0, 0); err != nil {
		return err
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return err
	}
	in := bufio.NewReader(r.Stdin)
	raw := first(r.g.URL, r.Getenv("PAAS_URL"), cfg.URL)
	if raw == "" {
		fmt.Fprint(r.Stderr, "Platform URL (e.g. https://paas.example.com): ")
		if raw, err = readLine(in); err != nil {
			return err
		}
	}
	base, err := NormalizeURL(raw)
	if err != nil {
		return err
	}
	token := first(r.g.Token, r.Getenv("PAAS_TOKEN"))
	if token == "" {
		fmt.Fprintf(r.Stderr, "Create a personal API token at %s/tokens and paste it here.\nToken: ", base)
		if token, err = readLine(in); err != nil {
			return err
		}
		if token == "" {
			return errors.New("no token given")
		}
	}
	c := &Client{BaseURL: base, Token: token, HTTP: r.HTTP}
	var who me
	if _, err := c.Do(r.ctx, "GET", "/api/me", nil, &who); err != nil {
		return fmt.Errorf("verifying the token: %w", err)
	}
	path, err := SaveConfig(Config{URL: base, Token: token})
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeJSON(r.Stdout, map[string]any{"url": base, "login": who.Login, "admin": who.Admin, "config": path})
	}
	fmt.Fprintf(r.Stdout, "Logged in to %s as %s.\nCredentials saved to %s\n", base, displayLogin(who), path)
	return nil
}

func displayLogin(m me) string {
	switch {
	case m.Login == "" && m.Admin:
		return "the admin token"
	case m.Name != "" && m.Name != m.Login:
		return m.Login + " (" + m.Name + ")"
	}
	return m.Login
}

func cmdLogout(r *runner, args []string) error {
	if _, err := r.parse(r.flags(), args, 0, 0); err != nil {
		return err
	}
	cfg, _, _ := LoadConfig()
	removed, path, err := RemoveConfig()
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeJSON(r.Stdout, map[string]any{"logged_out": removed, "config": path})
	}
	if !removed {
		fmt.Fprintln(r.Stdout, "Not logged in.")
		return nil
	}
	fmt.Fprintf(r.Stdout, "Logged out: removed %s\n", path)
	if cfg.URL != "" {
		fmt.Fprintf(r.Stdout, "The token itself stays valid until revoked at %s/tokens\n", strings.TrimRight(cfg.URL, "/"))
	}
	return nil
}

func cmdWhoami(r *runner, args []string) error {
	if _, err := r.parse(r.flags(), args, 0, 0); err != nil {
		return err
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	var who me
	raw, err := c.Do(r.ctx, "GET", "/api/me", nil, &who)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	fmt.Fprintf(r.Stdout, "%s on %s\n", displayLogin(who), c.BaseURL)
	if who.Admin {
		fmt.Fprintln(r.Stdout, "Admin: owner rights on every team")
	}
	if len(who.Teams) == 0 {
		fmt.Fprintln(r.Stdout, "No teams.")
		return nil
	}
	fmt.Fprintln(r.Stdout)
	tw := newTable(r.Stdout)
	fmt.Fprintln(tw, "TEAM\tNAME\tROLE")
	for _, t := range who.Teams {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Slug, orDash(t.Name), orDash(t.Role))
	}
	return tw.Flush()
}

// ---- apps and deployments ----

func cmdLs(r *runner, args []string) error {
	if _, err := r.parse(r.flags(), args, 0, 0); err != nil {
		return err
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	var apps []app
	raw, err := c.Do(r.ctx, "GET", "/api/apps", nil, &apps)
	if err != nil {
		return err
	}
	// The last deployment of each app, a few requests at a time.
	last := make([]*deployment, len(apps))
	lastRaw := make([]json.RawMessage, len(apps))
	errs := make([]error, len(apps))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range apps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var deps []json.RawMessage
			if _, err := c.Do(r.ctx, "GET", appPath(apps[i].Name, "deployments")+"?limit=1", nil, &deps); err != nil {
				errs[i] = err
				return
			}
			if len(deps) > 0 {
				var d deployment
				if json.Unmarshal(deps[0], &d) == nil {
					last[i], lastRaw[i] = &d, deps[0]
				}
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && r.ctx.Err() != nil {
			return r.ctx.Err()
		}
	}

	if r.g.JSON {
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		for i := range items {
			items[i]["last_deployment"] = json.RawMessage("null")
			if lastRaw[i] != nil {
				items[i]["last_deployment"] = lastRaw[i]
			}
		}
		if items == nil {
			items = []map[string]json.RawMessage{}
		}
		return writeJSON(r.Stdout, items)
	}
	if len(apps) == 0 {
		fmt.Fprintln(r.Stdout, "No apps yet. Import a repository with `paas import <owner/repo>`.")
		return nil
	}
	now := r.Now()
	tw := newTable(r.Stdout)
	fmt.Fprintln(tw, "NAME\tPRODUCTION URL\tLAST DEPLOYMENT\tSTATUS\tBRANCH\tAGE")
	for i, a := range apps {
		switch d := last[i]; {
		case errs[i] != nil:
			fmt.Fprintf(tw, "%s\t%s\t?\t(error: %v)\t\t\n", a.Name, a.ProductionURL, errs[i])
		case d == nil:
			fmt.Fprintf(tw, "%s\t%s\t-\tno deployments\t-\t-\n", a.Name, a.ProductionURL)
		default:
			fmt.Fprintf(tw, "%s\t%s\t#%d %s\t%s\t%s\t%s\n", a.Name, a.ProductionURL, d.ID, shortSHA(d.CommitSHA),
				d.statusText(), orDash(d.Branch), relTime(now, d.CreatedAt))
		}
	}
	return tw.Flush()
}

func cmdDeployments(r *runner, args []string) error {
	fs := r.flags()
	limit := fs.Int("limit", 20, "number of deployments to show (1-100)")
	pos, err := r.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *limit < 1 || *limit > 100 {
		return usagef("--limit must be between 1 and 100")
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	var deps []deployment
	raw, err := c.Do(r.ctx, "GET", appPath(pos[0], "deployments")+"?limit="+strconv.Itoa(*limit), nil, &deps)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	if len(deps) == 0 {
		fmt.Fprintf(r.Stdout, "%s has no deployments yet. Push a commit to its repository.\n", pos[0])
		return nil
	}
	// Which deployments the aliases point at (production, branch previews).
	tags := map[int64][]string{}
	var a app
	if _, err := c.Do(r.ctx, "GET", appPath(pos[0]), nil, &a); err == nil {
		for _, al := range a.Aliases {
			tag := al.Kind
			if al.Kind != "production" && al.Branch != "" {
				tag = "preview:" + al.Branch
			}
			tags[al.DeploymentID] = append(tags[al.DeploymentID], tag)
		}
	}
	now := r.Now()
	tw := newTable(r.Stdout)
	fmt.Fprintln(tw, "ID\tCOMMIT\tBRANCH\tSTATUS\tALIAS\tAGE\tBUILD\tMESSAGE")
	for _, d := range deps {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, shortSHA(d.CommitSHA), orDash(d.Branch),
			d.statusText(), orDash(strings.Join(tags[d.ID], ",")), relTime(now, d.CreatedAt), d.buildTime(),
			oneLine(d.CommitMessage, 50))
	}
	return tw.Flush()
}

func cmdInspect(r *runner, args []string) error {
	pos, err := r.parse(r.flags(), args, 1, 1)
	if err != nil {
		return err
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	var d deployment
	raw, err := c.Do(r.ctx, "GET", "/api/deployments/"+strconv.FormatInt(id, 10), nil, &d)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	now := r.Now()
	tw := newTable(r.Stdout)
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "%s\t%s\n", k, v)
		}
	}
	row("Deployment", "#"+strconv.FormatInt(d.ID, 10))
	row("App", d.AppName)
	row("Status", d.statusText())
	row("Error", d.Error)
	row("URL", d.URL)
	row("Repository", d.Repo)
	row("Branch", d.Branch)
	row("Commit", d.CommitSHA)
	row("Message", oneLine(d.CommitMessage, 72))
	row("Image", d.Image)
	row("Created", d.CreatedAt.Local().Format(time.DateTime)+" ("+relTime(now, d.CreatedAt)+")")
	if d.FinishedAt != nil {
		row("Finished", d.FinishedAt.Local().Format(time.DateTime)+" ("+relTime(now, *d.FinishedAt)+")")
		row("Build time", d.buildTime())
	}
	if d.Attempts > 1 {
		row("Attempts", strconv.Itoa(d.Attempts))
	}
	if d.RetiredAt != nil {
		row("Retired", relTime(now, *d.RetiredAt)+" "+d.RetireReason)
	}
	return tw.Flush()
}

func cmdRollback(r *runner, args []string) error {
	pos, err := r.parse(r.flags(), args, 2, 2)
	if err != nil {
		return err
	}
	id, err := parseID(pos[1])
	if err != nil {
		return err
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	var al alias
	raw, err := c.Do(r.ctx, "POST", appPath(pos[0], "rollback"), map[string]int64{"deployment_id": id}, &al)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	fmt.Fprintf(r.Stdout, "Rolled back %s: production (%s) now serves deployment #%d.\n", pos[0], al.Hostname, id)
	return nil
}

func cmdOpen(r *runner, args []string) error {
	fs := r.flags()
	printOnly := fs.Bool("print", false, "only print the URL, do not open a browser")
	pos, err := r.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	var a app
	if _, err := c.Do(r.ctx, "GET", appPath(pos[0]), nil, &a); err != nil {
		return err
	}
	if r.g.JSON {
		return writeJSON(r.Stdout, map[string]string{"app": a.Name, "production_url": a.ProductionURL})
	}
	fmt.Fprintln(r.Stdout, a.ProductionURL)
	if *printOnly || r.OpenURL == nil {
		return nil
	}
	if err := r.OpenURL(a.ProductionURL); err != nil {
		fmt.Fprintf(r.Stderr, "Could not open a browser (%v); open the URL above yourself.\n", err)
	}
	return nil
}

// ---- import ----

func cmdImport(r *runner, args []string) error {
	fs := r.flags()
	name := fs.String("name", "", "app `name` (default: derived from the repository name)")
	teamSlug := fs.String("team", "", "team `slug` (default: your team that may import the repository)")
	branch := fs.String("branch", "", "production `branch` (default: the repository's default branch)")
	pos, err := r.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	repo := pos[0]
	if o, n, ok := strings.Cut(repo, "/"); !ok || o == "" || n == "" || strings.Contains(n, "/") {
		return usagef("repository must look like owner/repo, got %q", repo)
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	body := map[string]string{"repo": repo}
	if *name != "" {
		body["name"] = *name
	}
	if *teamSlug != "" {
		body["team"] = *teamSlug
	}
	if *branch != "" {
		body["production_branch"] = *branch
	}
	var res struct {
		App        app         `json:"app"`
		Deployment *deployment `json:"deployment"`
		Warning    string      `json:"warning"`
	}
	raw, err := c.Do(r.ctx, "POST", "/api/apps/import", body, &res)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	fmt.Fprintf(r.Stdout, "Imported %s as %s (production branch %s).\nProduction URL: %s\n",
		repo, res.App.Name, res.App.ProductionBranch, res.App.ProductionURL)
	if d := res.Deployment; d != nil {
		fmt.Fprintf(r.Stdout, "First deployment #%d (%s) is %s. Follow it with: paas logs %d -f\n",
			d.ID, shortSHA(d.CommitSHA), d.Status, d.ID)
	}
	if res.Warning != "" {
		fmt.Fprintln(r.Stderr, "Warning:", res.Warning)
	}
	return nil
}

// ---- env ----

func cmdEnv(r *runner, args []string) error {
	pos, err := r.parse(r.flags(), args, 1, -1)
	if err != nil {
		return err
	}
	sub, rest := pos[0], pos[1:]
	if len(rest) == 0 {
		return usagef("missing app name")
	}
	appName, rest := rest[0], rest[1:]
	changes := map[string]*string{}
	switch sub {
	case "ls", "list":
		if len(rest) > 0 {
			return usagef("too many arguments: %s", strings.Join(rest, " "))
		}
	case "set", "add":
		if len(rest) == 0 {
			return usagef("give at least one KEY=VALUE")
		}
		for _, kv := range rest {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return usagef("%q is not KEY=VALUE", kv)
			}
			changes[k] = &v
		}
	case "rm", "remove", "unset":
		if len(rest) == 0 {
			return usagef("give at least one KEY")
		}
		for _, k := range rest {
			if strings.Contains(k, "=") {
				return usagef("%q: rm takes keys only", k)
			}
			changes[k] = nil
		}
	default:
		return usagef("unknown env subcommand %q (want ls, set or rm)", sub)
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	var view struct {
		Keys []string `json:"keys"`
	}
	var raw []byte
	if len(changes) == 0 {
		raw, err = c.Do(r.ctx, "GET", appPath(appName, "env"), nil, &view)
	} else {
		raw, err = c.Do(r.ctx, "PUT", appPath(appName, "env"), changes, &view)
	}
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	switch sub {
	case "set", "add":
		fmt.Fprintf(r.Stdout, "Set %s on %s. Changes apply to the next deployment.\n", strings.Join(keys, ", "), appName)
	case "rm", "remove", "unset":
		fmt.Fprintf(r.Stdout, "Removed %s from %s. Changes apply to the next deployment.\n", strings.Join(keys, ", "), appName)
	default:
		if len(view.Keys) == 0 {
			fmt.Fprintf(r.Stdout, "%s has no environment variables.\n", appName)
			return nil
		}
		fmt.Fprintln(r.Stdout, "KEY")
		for _, k := range view.Keys {
			fmt.Fprintln(r.Stdout, k)
		}
		fmt.Fprintln(r.Stderr, "(values are write-only and never returned by the API)")
	}
	return nil
}

// ---- domains ----

func cmdDomains(r *runner, args []string) error {
	pos, err := r.parse(r.flags(), args, 2, 3)
	if err != nil {
		return err
	}
	sub, appName := pos[0], pos[1]
	host := ""
	if len(pos) == 3 {
		host = strings.ToLower(strings.TrimSpace(pos[2]))
	}
	needHost := sub == "add" || sub == "rm" || sub == "remove" || sub == "verify"
	switch {
	case sub != "ls" && sub != "list" && !needHost:
		return usagef("unknown domains subcommand %q (want ls, add, verify or rm)", sub)
	case needHost && host == "":
		return usagef("missing hostname")
	case !needHost && host != "":
		return usagef("too many arguments: %s", host)
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	switch sub {
	case "rm", "remove":
		if _, err := c.Do(r.ctx, "DELETE", appPath(appName, "domains", url.PathEscape(host)), nil, nil); err != nil {
			return err
		}
		if r.g.JSON {
			return writeJSON(r.Stdout, map[string]any{"app": appName, "hostname": host, "removed": true})
		}
		fmt.Fprintf(r.Stdout, "Removed %s from %s.\n", host, appName)
		return nil
	case "add", "verify":
		var d domain
		var raw []byte
		if sub == "add" {
			raw, err = c.Do(r.ctx, "POST", appPath(appName, "domains"), map[string]string{"hostname": host}, &d)
		} else {
			raw, err = c.Do(r.ctx, "POST", appPath(appName, "domains", url.PathEscape(host), "verify"), nil, &d)
		}
		if err != nil {
			return err
		}
		if r.g.JSON {
			return writeRawJSON(r.Stdout, raw)
		}
		verb := "Added"
		if sub == "verify" {
			verb = "Checked"
		}
		fmt.Fprintf(r.Stdout, "%s %s on %s: %s", verb, d.Hostname, appName, d.Status)
		if d.Error != "" {
			fmt.Fprintf(r.Stdout, " (%s)", d.Error)
		}
		fmt.Fprintln(r.Stdout)
		if d.Status != "verified" && d.Status != "active" && len(d.DNSRecords) > 0 {
			fmt.Fprintln(r.Stdout, "\nCreate one of these DNS records, then run `paas domains verify "+appName+" "+d.Hostname+"`:")
			tw := newTable(r.Stdout)
			fmt.Fprintln(tw, "TYPE\tNAME\tVALUE\tNOTE")
			for _, rec := range d.DNSRecords {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", rec.Type, rec.Name, rec.Value, rec.Note)
			}
			return tw.Flush()
		}
		return nil
	}
	var list []domain
	raw, err := c.Do(r.ctx, "GET", appPath(appName, "domains"), nil, &list)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return writeRawJSON(r.Stdout, raw)
	}
	if len(list) == 0 {
		fmt.Fprintf(r.Stdout, "%s has no custom domains. Add one with `paas domains add %s <host>`.\n", appName, appName)
		return nil
	}
	tw := newTable(r.Stdout)
	fmt.Fprintln(tw, "HOSTNAME\tSTATUS\tSERVING\tERROR")
	for _, d := range list {
		serving := "no"
		if d.Serving {
			serving = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Hostname, d.Status, serving, orDash(oneLine(d.Error, 60)))
	}
	return tw.Flush()
}
