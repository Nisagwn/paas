package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// GitHubApp holds the GitHub App settings (Faz 15). The App's login uses
// PAAS_GITHUB_OAUTH_CLIENT_ID / _SECRET (its client id and secret) and its
// webhook PAAS_GITHUB_WEBHOOK_SECRET.
type GitHubApp struct {
	// App ID from the App's settings page.
	ID int64
	// PEM private key: PAAS_GITHUB_APP_PRIVATE_KEY_FILE or
	// PAAS_GITHUB_APP_PRIVATE_KEY. Parsed at startup (github.ParsePrivateKey).
	PrivateKey []byte
	// The App's URL name: https://github.com/apps/<slug>/installations/new.
	Slug string
}

// Enabled reports whether the App is configured. It then replaces the
// personal access token for clones, statuses and comments.
func (a GitHubApp) Enabled() bool { return a.ID != 0 && len(a.PrivateKey) > 0 }

// loadGitHubApp reads the Faz 15 settings. A key file that does not exist
// counts as unset, so a Kubernetes Secret mounted with optional: true works
// on installs without an App. Anything partial is an error.
func (c *Config) loadGitHubApp() error {
	a := GitHubApp{Slug: strings.TrimSpace(os.Getenv("PAAS_GITHUB_APP_SLUG"))}
	if v := strings.TrimSpace(os.Getenv("PAAS_GITHUB_APP_ID")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return errors.New("PAAS_GITHUB_APP_ID must be a positive integer")
		}
		a.ID = id
	}

	file, inline := os.Getenv("PAAS_GITHUB_APP_PRIVATE_KEY_FILE"), os.Getenv("PAAS_GITHUB_APP_PRIVATE_KEY")
	if file != "" {
		b, err := os.ReadFile(file)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return fmt.Errorf("PAAS_GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
		default:
			a.PrivateKey = b
		}
	}
	if strings.TrimSpace(inline) != "" {
		if len(a.PrivateKey) > 0 {
			return errors.New("set PAAS_GITHUB_APP_PRIVATE_KEY_FILE or PAAS_GITHUB_APP_PRIVATE_KEY, not both")
		}
		a.PrivateKey = []byte(inline)
	}

	hasKey := len(a.PrivateKey) > 0
	switch {
	case a.ID == 0 && !hasKey && a.Slug == "":
	case a.ID == 0:
		return errors.New("GitHub App: PAAS_GITHUB_APP_ID is not set")
	case !hasKey:
		if file != "" {
			return fmt.Errorf("GitHub App: private key file %s not found", file)
		}
		return errors.New("GitHub App: set PAAS_GITHUB_APP_PRIVATE_KEY_FILE or PAAS_GITHUB_APP_PRIVATE_KEY")
	case a.Slug == "":
		return errors.New("GitHub App: PAAS_GITHUB_APP_SLUG is not set")
	}
	c.GitHubApp = a
	return nil
}
