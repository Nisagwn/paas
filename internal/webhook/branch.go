package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// zeroSHA is the "after" of a push that deleted its branch.
const zeroSHA = "0000000000000000000000000000000000000000"

// ErrBranchDeleted is returned by ParsePush for a branch deletion. It wraps
// ErrIgnored: nothing is deployed, but the branch's previews are retired.
var ErrBranchDeleted = fmt.Errorf("branch deleted: %w", ErrIgnored)

// PullRequestEvent holds the fields of a GitHub "pull_request" payload that
// paas needs.
type PullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Merged bool `json:"merged"`
		Head   struct {
			Ref  string `json:"ref"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"` // null when the fork was deleted
		} `json:"head"`
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
}

// ClosedPR is a closed pull request whose head branch lives in the same
// repository, so its previews can be retired.
type ClosedPR struct {
	Repo   string
	Branch string
	Number int
	Merged bool
}

// ParseClosedPR decodes a pull_request payload. Anything other than a
// closed pull request from a branch of the same repository returns
// ErrIgnored: a fork's branch never had a preview here.
func ParseClosedPR(body []byte) (ClosedPR, error) {
	var ev PullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return ClosedPR{}, err
	}
	if ev.Action != "closed" {
		return ClosedPR{}, ErrIgnored
	}
	pr := ev.PullRequest
	base := pr.Base.Repo.FullName
	if base == "" || pr.Head.Ref == "" {
		return ClosedPR{}, errors.New("pull_request payload missing base repository or head ref")
	}
	if pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, base) {
		return ClosedPR{}, ErrIgnored
	}
	return ClosedPR{Repo: base, Branch: pr.Head.Ref, Number: ev.Number, Merged: pr.Merged}, nil
}
