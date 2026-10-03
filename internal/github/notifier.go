package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

// DefaultContext is the commit status context shown on GitHub.
const DefaultContext = "paas/deploy"

// maxCommentError bounds the error text quoted in a PR comment.
const maxCommentError = 1500

// Notifier implements worker.Notifier: it sets a commit status for every
// deployment and keeps one comment per app up to date on the open pull
// requests of the deployed branch.
type Notifier struct {
	Client *Client
	// PublicURL is where the control plane is reachable; deployment pages
	// are linked as <PublicURL>/deployments/<id>.
	PublicURL string
	// Context of the commit status (DefaultContext when empty).
	Context string
	Log     *slog.Logger
}

var _ worker.Notifier = (*Notifier)(nil)

// DeploymentStarted marks the commit "pending".
func (n *Notifier) DeploymentStarted(ctx context.Context, d store.Deployment) error {
	return n.Client.CreateCommitStatus(ctx, d.Repo, d.CommitSHA, Status{
		State:       StatePending,
		TargetURL:   n.DeploymentPage(d.ID),
		Description: "Build başladı",
		Context:     n.context(),
	})
}

// DeploymentFinished sets the final commit status and updates the preview
// comment on the branch's open pull requests.
func (n *Notifier) DeploymentFinished(ctx context.Context, d store.Deployment, r worker.Result) error {
	s := Status{Context: n.context()}
	if r.Status == store.StatusReady {
		s.State, s.TargetURL = StateSuccess, r.URL
		s.Description = "Deploy hazır: " + strings.TrimPrefix(r.URL, "https://")
	} else {
		s.State, s.TargetURL = StateFailure, n.DeploymentPage(d.ID)
		s.Description = "Deploy başarısız: " + oneLine(r.Error)
	}
	statusErr := n.Client.CreateCommitStatus(ctx, d.Repo, d.CommitSHA, s)
	return errors.Join(statusErr, n.commentOnPullRequests(ctx, d, r))
}

func (n *Notifier) commentOnPullRequests(ctx context.Context, d store.Deployment, r worker.Result) error {
	prs, err := n.Client.OpenPullRequests(ctx, d.Repo, d.Branch)
	if err != nil {
		return err
	}
	marker := CommentMarker(d.AppName)
	body := n.CommentBody(d, r)
	var errs []error
	for _, pr := range prs {
		// A late build of an older commit must not overwrite the comment of
		// a newer one; the newer deployment comments when it finishes.
		if !strings.EqualFold(pr.Head.SHA, d.CommitSHA) {
			n.log().Info("github: PR head moved on, comment skipped", "pr", pr.Number,
				"deployment", d.ID, "head", naming.ShortSHA(pr.Head.SHA))
			continue
		}
		c, created, err := n.Client.UpsertComment(ctx, d.Repo, pr.Number, marker, body)
		if err != nil {
			errs = append(errs, fmt.Errorf("PR #%d: %w", pr.Number, err))
			continue
		}
		n.log().Info("github: PR comment updated", "pr", pr.Number, "deployment", d.ID,
			"created", created, "url", c.HTMLURL)
	}
	return errors.Join(errs...)
}

// CommentMarker identifies paas' comment for an app in a PR thread.
func CommentMarker(app string) string {
	return "<!-- paas:preview:" + app + " -->"
}

// CommentBody renders the PR comment for a finished deployment.
func (n *Notifier) CommentBody(d store.Deployment, r worker.Result) string {
	var b strings.Builder
	b.WriteString(CommentMarker(d.AppName) + "\n")
	commit := "`" + naming.ShortSHA(d.CommitSHA) + "`"
	if d.CommitMessage != "" {
		commit += " " + escapeCell(d.CommitMessage)
	}
	page := n.DeploymentPage(d.ID)

	if r.Status == store.StatusReady {
		fmt.Fprintf(&b, "### ✅ Preview hazır: `%s`\n\n", d.AppName)
		b.WriteString("| | |\n|---|---|\n")
		fmt.Fprintf(&b, "| **Preview** | %s |\n", r.PreviewURL)
		if r.ProductionURL != "" {
			fmt.Fprintf(&b, "| **Production** | %s |\n", r.ProductionURL)
		}
		fmt.Fprintf(&b, "| **Deployment** | %s |\n", r.URL)
		fmt.Fprintf(&b, "| **Commit** | %s |\n", commit)
		b.WriteString("| **Durum** | ✅ Hazır |\n")
		if page != "" {
			fmt.Fprintf(&b, "| **Loglar** | [Deployment #%d](%s) |\n", d.ID, page)
		}
	} else {
		fmt.Fprintf(&b, "### ❌ Deploy başarısız: `%s`\n\n", d.AppName)
		b.WriteString("| | |\n|---|---|\n")
		fmt.Fprintf(&b, "| **Commit** | %s |\n", commit)
		b.WriteString("| **Durum** | ❌ Başarısız |\n")
		if page != "" {
			fmt.Fprintf(&b, "| **Loglar** | [Deployment #%d](%s) |\n", d.ID, page)
		}
		if r.Error != "" {
			msg := strings.ReplaceAll(Truncate(r.Error, maxCommentError), "```", "'''")
			fmt.Fprintf(&b, "\n**Hata:**\n\n```text\n%s\n```\n", msg)
		}
	}
	b.WriteString("\n<sub>Bu yorum her push'ta paas tarafından güncellenir.</sub>\n")
	return b.String()
}

// DeploymentPage is the web UI page of a deployment, with its logs.
func (n *Notifier) DeploymentPage(id int64) string {
	if n.PublicURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/deployments/%d", strings.TrimRight(n.PublicURL, "/"), id)
}

func (n *Notifier) context() string {
	if n.Context == "" {
		return DefaultContext
	}
	return n.Context
}

func (n *Notifier) log() *slog.Logger {
	if n.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return n.Log
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// escapeCell keeps text from breaking a Markdown table row or injecting HTML.
func escapeCell(s string) string {
	s = oneLine(s)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}
