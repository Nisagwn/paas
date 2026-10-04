package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadGitHubApp(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "github-app.pem")
	os.WriteFile(keyFile, []byte("-----BEGIN RSA PRIVATE KEY-----\n..."), 0o600)
	missing := filepath.Join(t.TempDir(), "absent.pem")

	for _, tc := range []struct {
		name    string
		env     map[string]string
		enabled bool
		err     string
	}{
		{name: "not configured", env: map[string]string{}},
		{name: "optional key file absent", env: map[string]string{"PAAS_GITHUB_APP_PRIVATE_KEY_FILE": missing}},
		{name: "file", enabled: true, env: map[string]string{
			"PAAS_GITHUB_APP_ID": "123", "PAAS_GITHUB_APP_SLUG": "paas-dev", "PAAS_GITHUB_APP_PRIVATE_KEY_FILE": keyFile}},
		{name: "inline", enabled: true, env: map[string]string{
			"PAAS_GITHUB_APP_ID": "123", "PAAS_GITHUB_APP_SLUG": "paas-dev", "PAAS_GITHUB_APP_PRIVATE_KEY": `-----BEGIN…\n…`}},
		{name: "id without key", err: "PRIVATE_KEY", env: map[string]string{"PAAS_GITHUB_APP_ID": "123", "PAAS_GITHUB_APP_SLUG": "x"}},
		{name: "id, key file absent", err: "not found", env: map[string]string{
			"PAAS_GITHUB_APP_ID": "123", "PAAS_GITHUB_APP_SLUG": "x", "PAAS_GITHUB_APP_PRIVATE_KEY_FILE": missing}},
		{name: "key without id", err: "PAAS_GITHUB_APP_ID", env: map[string]string{"PAAS_GITHUB_APP_PRIVATE_KEY": "k"}},
		{name: "slug alone", err: "PAAS_GITHUB_APP_ID", env: map[string]string{"PAAS_GITHUB_APP_SLUG": "x"}},
		{name: "no slug", err: "PAAS_GITHUB_APP_SLUG", env: map[string]string{
			"PAAS_GITHUB_APP_ID": "123", "PAAS_GITHUB_APP_PRIVATE_KEY": "k"}},
		{name: "bad id", err: "positive integer", env: map[string]string{"PAAS_GITHUB_APP_ID": "abc"}},
		{name: "both keys", err: "not both", env: map[string]string{"PAAS_GITHUB_APP_ID": "1", "PAAS_GITHUB_APP_SLUG": "x",
			"PAAS_GITHUB_APP_PRIVATE_KEY_FILE": keyFile, "PAAS_GITHUB_APP_PRIVATE_KEY": "k"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"PAAS_GITHUB_APP_ID", "PAAS_GITHUB_APP_SLUG",
				"PAAS_GITHUB_APP_PRIVATE_KEY_FILE", "PAAS_GITHUB_APP_PRIVATE_KEY"} {
				t.Setenv(k, tc.env[k])
			}
			var c Config
			err := c.loadGitHubApp()
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want one mentioning %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.GitHubApp.Enabled() != tc.enabled {
				t.Fatalf("enabled = %v, want %v (%+v)", c.GitHubApp.Enabled(), tc.enabled, c.GitHubApp)
			}
			if tc.enabled && (c.GitHubApp.ID != 123 || c.GitHubApp.Slug != "paas-dev" || len(c.GitHubApp.PrivateKey) == 0) {
				t.Fatalf("app = %+v", c.GitHubApp)
			}
		})
	}
}
