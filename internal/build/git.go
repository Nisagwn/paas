package build

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Checkout fetches exactly one commit (depth 1) into dir, which must not
// exist or be empty. Fetching by SHA instead of by branch means a push that
// lands while we are queued cannot change what gets built.
//
// token, if set, is sent as an HTTP Authorization header through git's
// environment so it never appears in argv, the remote URL or the logs.
func Checkout(ctx context.Context, repoURL, sha, token, dir string, out io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // fail instead of asking for a password
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
		)
	}
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		var stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = out, io.MultiWriter(out, &stderr)
		if err := cmd.Run(); err != nil {
			// git's last stderr line ("fatal: …") says far more than "exit status 128".
			if msg := lastLine(stderr.String()); msg != "" {
				return fmt.Errorf("git %s: %s", args[0], msg)
			}
			return fmt.Errorf("git %s: %w", args[0], err)
		}
		return nil
	}

	if err := git("init", "-q"); err != nil {
		return err
	}
	if err := git("fetch", "--depth=1", "--no-tags", "--no-progress", repoURL, sha); err != nil {
		return err
	}
	if err := git("-c", "advice.detachedHead=false", "checkout", "-q", "FETCH_HEAD"); err != nil {
		return err
	}

	head, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse: %w", err)
	}
	if got := strings.TrimSpace(string(head)); got != sha {
		return fmt.Errorf("checked out %s, expected %s", got, sha)
	}
	// The build context never needs history; leaving .git out keeps it small.
	return os.RemoveAll(filepath.Join(dir, ".git"))
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
