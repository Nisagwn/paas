// Package webhook verifies and parses GitHub webhook deliveries.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

var (
	ErrBadSignature = errors.New("invalid webhook signature")
	shaRe           = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// VerifySignature checks GitHub's X-Hub-Signature-256 header
// ("sha256=<hex hmac of body>") in constant time.
func VerifySignature(secret string, body []byte, header string) error {
	const prefix = "sha256="
	if secret == "" || !strings.HasPrefix(header, prefix) {
		return ErrBadSignature
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrBadSignature
	}
	return nil
}

// Sign returns the header value GitHub would send for body. Used in tests and
// by the local `make webhook` helper.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// PushEvent holds the fields of a GitHub "push" payload that paas needs.
type PushEvent struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	HeadCommit *struct {
		Message string `json:"message"`
	} `json:"head_commit"`
}

// Push is a validated push that should trigger a deployment.
type Push struct {
	Repo    string
	Branch  string
	SHA     string
	Message string
}

// ErrIgnored marks pushes that are valid but should not deploy
// (tag pushes, branch deletions).
var ErrIgnored = errors.New("push ignored")

// ParsePush decodes a push payload and decides whether it should deploy.
func ParsePush(body []byte) (Push, error) {
	var ev PushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return Push{}, err
	}
	const headsPrefix = "refs/heads/"
	if !strings.HasPrefix(ev.Ref, headsPrefix) {
		return Push{}, ErrIgnored // tags and other refs
	}
	if ev.Deleted {
		return Push{}, ErrIgnored // branch deleted; preview cleanup comes in Faz 7
	}
	if !shaRe.MatchString(ev.After) || ev.Repository.FullName == "" {
		return Push{}, errors.New("push payload missing commit sha or repository")
	}
	p := Push{
		Repo:   ev.Repository.FullName,
		Branch: strings.TrimPrefix(ev.Ref, headsPrefix),
		SHA:    ev.After,
	}
	if ev.HeadCommit != nil {
		// Keep only the subject line; full messages can be long.
		p.Message, _, _ = strings.Cut(ev.HeadCommit.Message, "\n")
	}
	return p, nil
}
