package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Config is what `paas login` stores: the platform URL and a personal API
// token. It lives in os.UserConfigDir()/paas/config.json with mode 0600.
type Config struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// ConfigPath is the location of the config file.
func ConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the config directory: %w", err)
	}
	return filepath.Join(dir, "paas", "config.json"), nil
}

// LoadConfig reads the config file; a missing file is an empty Config.
func LoadConfig() (Config, string, error) {
	var c Config
	path, err := ConfigPath()
	if err != nil {
		return c, "", err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, path, nil
	}
	if err != nil {
		return c, path, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, path, fmt.Errorf("%s is not valid JSON (delete it and run `paas login` again): %w", path, err)
	}
	return c, path, nil
}

// SaveConfig writes c readable by the current user only.
func SaveConfig(c Config) (string, error) {
	path, err := ConfigPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return path, err
	}
	// Write a sibling file and rename it, so a crash never leaves a
	// truncated config; the temp file is created 0600 from the start.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return path, fmt.Errorf("writing %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && !isUnsupported(err) {
		tmp.Close()
		return path, fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return path, fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return path, fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return path, fmt.Errorf("writing %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)
	return path, nil
}

// isUnsupported: Windows has no Unix permission bits for File.Chmod.
func isUnsupported(err error) bool {
	return errors.Is(err, errors.ErrUnsupported)
}

// RemoveConfig deletes the config file and reports whether there was one.
func RemoveConfig() (bool, string, error) {
	path, err := ConfigPath()
	if err != nil {
		return false, "", err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, path, nil
	}
	if err != nil {
		return false, path, fmt.Errorf("removing %s: %w", path, err)
	}
	return true, path, nil
}

// NormalizeURL turns "paas.example.com/" into "https://paas.example.com".
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("the platform URL is empty")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid platform URL %q: want something like https://paas.example.com", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid platform URL %q: remove the query string", raw)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}
