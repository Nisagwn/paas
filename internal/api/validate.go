package api

import (
	"errors"
	"strconv"
	"strings"
)

// Validation shared with the web UI, so both front doors accept the same input.

// ValidRepo reports whether repo looks like GitHub's "owner/repo".
func ValidRepo(repo string) bool { return repoRe.MatchString(repo) }

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
