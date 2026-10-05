package api

import (
	"errors"
	"strconv"
	"strings"

	"github.com/nisagwn/paas/internal/store"
)

// Validation shared with the web UI, so both front doors accept the same input.

// ValidRepo reports whether repo looks like GitHub's "owner/repo".
func ValidRepo(repo string) bool { return repoRe.MatchString(repo) }

// CheckEnvScope validates the target and branch of a variable change;
// target "" means both environments.
func CheckEnvScope(target, branch string) error {
	switch {
	case target != "" && !store.ValidEnvTarget(target):
		return errors.New(`target must be "production", "preview" or "all"`)
	case branch != "" && target != store.EnvPreview:
		return errors.New(`git_branch needs target "preview"`)
	case len(branch) > 255:
		return errors.New("git_branch is too long")
	}
	return nil
}

// CheckEnvVar validates one change of PUT /api/apps/{name}/env; a nil value
// is a deletion.
func CheckEnvVar(key string, value *string) error {
	switch {
	case !envKeyRe.MatchString(key):
		return errors.New("invalid variable name " + strconv.Quote(key))
	case key == "PORT" || strings.HasPrefix(key, "PAAS_"):
		return errors.New(key + " is set by the platform")
	case value != nil && len(*value) > maxEnvValue:
		return errors.New(key + " is longer than 32 KiB")
	}
	return nil
}
