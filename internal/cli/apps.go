package cli

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
)

// Faz 28: `paas apps delete <app> [--yes]`.

func cmdApps(r *runner, args []string) error {
	fs := r.flags()
	yes := fs.Bool("yes", false, "delete without asking for the app name")
	pos, err := r.parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	sub, name := pos[0], pos[1]
	if sub != "delete" && sub != "rm" && sub != "remove" {
		return usagef("unknown apps subcommand %q (want delete)", sub)
	}
	if !*yes {
		fmt.Fprintf(r.Stderr, "This permanently deletes %s: its deployments and URLs, custom domains, databases with\n"+
			"their data and backups, environment variables and settings. It cannot be undone.\n"+
			"Type the app name to confirm: ", name)
		typed, err := readLine(bufio.NewReader(r.Stdin))
		if err != nil {
			return &hintError{msg: "no confirmation read", hint: "Pass --yes when stdin is not a terminal.", err: err}
		}
		if typed != name {
			return errors.New("the typed name does not match; nothing was deleted")
		}
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	raw, err := c.Do(r.ctx, "DELETE", appPath(name), map[string]string{"confirm": name}, nil)
	if err != nil {
		return err
	}
	pending := len(strings.TrimSpace(string(raw))) > 0 // 202 carries a body, 204 none
	if r.g.JSON {
		if pending {
			return writeRawJSON(r.Stdout, raw)
		}
		return writeJSON(r.Stdout, map[string]any{"app": name, "status": "deleted"})
	}
	if pending {
		fmt.Fprintf(r.Stdout, "Deleted %s. Its remaining cluster resources are being removed in the background;\n"+
			"the name can be used again in a few minutes.\n", name)
		return nil
	}
	fmt.Fprintf(r.Stdout, "Deleted %s.\n", name)
	return nil
}
