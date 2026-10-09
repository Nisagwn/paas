package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 25: preview comments mirrored to pull requests. Every thread on a
// deployment of a branch with open pull requests becomes one PR comment
// (found again by a hidden marker), updated when someone replies, resolves
// or reopens it, and deleted with the thread. It is best-effort: GitHub
// failures are logged and never reach the reviewer.

// commentQueue bounds the pending thread changes; more are dropped (and
// logged) rather than blocking a request.
const commentQueue = 256

// commentSyncTimeout bounds one thread's sync (PR lookup plus updates).
const commentSyncTimeout = 30 * time.Second

// maxSyncedReplies is how many of the latest replies a PR comment quotes.
const maxSyncedReplies = 20

// CommentSync implements api.CommentSync.
type CommentSync struct {
	Client *Client
	Store  *store.Store
	// PublicURL is where the control plane is reachable; threads link to
	// <PublicURL>/deployments/<id>/review?thread=<thread>.
	PublicURL string
	Log       *slog.Logger

	once    sync.Once
	queue   chan threadRef
	mu      sync.Mutex
	pending map[threadRef]bool
}

type threadRef struct{ deployment, thread int64 }

func (c *CommentSync) init() {
	c.once.Do(func() {
		c.queue = make(chan threadRef, commentQueue)
		c.pending = map[threadRef]bool{}
	})
}

// CommentChanged queues a thread for syncing; Run processes the queue.
// A thread already waiting is not queued twice: its sync reads the
// latest state anyway.
func (c *CommentSync) CommentChanged(deploymentID, threadID int64) {
	c.init()
	ref := threadRef{deploymentID, threadID}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending[ref] {
		return
	}
	select {
	case c.queue <- ref:
		c.pending[ref] = true
	default:
		c.log().Warn("github: comment sync queue full, change not mirrored", "deployment", deploymentID, "thread", threadID)
	}
}

// Run syncs queued threads one at a time (so a thread's create and resolve
// cannot race into two PR comments) until ctx ends.
func (c *CommentSync) Run(ctx context.Context) {
	c.init()
	for {
		select {
		case <-ctx.Done():
			return
		case ref := <-c.queue:
			c.mu.Lock()
			delete(c.pending, ref)
			c.mu.Unlock()
			sctx, cancel := context.WithTimeout(ctx, commentSyncTimeout)
			if err := c.Sync(sctx, ref.deployment, ref.thread); err != nil {
				c.log().Error("github: comment sync failed", "deployment", ref.deployment, "thread", ref.thread, "err", err)
			}
			cancel()
		}
	}
}

// Sync mirrors the current state of a thread to the open pull requests of
// its deployment's branch: created or updated, or deleted when the thread
// is gone.
func (c *CommentSync) Sync(ctx context.Context, deploymentID, threadID int64) error {
	d, err := c.Store.GetDeployment(ctx, deploymentID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if d.Branch == "" {
		return nil
	}
	prs, err := c.Client.OpenPullRequests(ctx, d.Repo, d.Branch)
	if errors.Is(err, ErrNoCredentials) {
		c.log().Info("github: no credentials for repository, comment not mirrored", "repo", d.Repo, "thread", threadID)
		return nil
	}
	if err != nil {
		return err
	}
	if len(prs) == 0 {
		return nil
	}
	marker := ThreadMarker(threadID)
	thread, err := c.Store.GetComment(ctx, threadID)
	deleted := errors.Is(err, store.ErrNotFound)
	if err != nil && !deleted {
		return err
	}
	var errs []error
	for _, pr := range prs {
		if deleted {
			found, err := c.Client.DeleteMarkedComment(ctx, d.Repo, pr.Number, marker)
			if err != nil {
				errs = append(errs, fmt.Errorf("PR #%d: %w", pr.Number, err))
			} else if found {
				c.log().Info("github: thread comment deleted", "pr", pr.Number, "thread", threadID)
			}
			continue
		}
		out, created, err := c.Client.UpsertComment(ctx, d.Repo, pr.Number, marker, c.ThreadBody(d, thread))
		if err != nil {
			errs = append(errs, fmt.Errorf("PR #%d: %w", pr.Number, err))
			continue
		}
		c.log().Info("github: thread comment synced", "pr", pr.Number, "thread", threadID,
			"created", created, "status", thread.Status, "url", out.HTMLURL)
	}
	return errors.Join(errs...)
}

// ThreadMarker identifies the PR comment of a thread.
func ThreadMarker(threadID int64) string {
	return fmt.Sprintf("<!-- paas:comment:%d -->", threadID)
}

// ReviewPage is the review page of a deployment, focused on a thread.
func (c *CommentSync) ReviewPage(deploymentID, threadID int64) string {
	if c.PublicURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/deployments/%d/review?thread=%d", strings.TrimRight(c.PublicURL, "/"), deploymentID, threadID)
}

// ThreadBody renders the PR comment of a thread.
func (c *CommentSync) ThreadBody(d store.Deployment, t store.Comment) string {
	var b strings.Builder
	b.WriteString(ThreadMarker(t.ID) + "\n")
	state := "🟢 Açık"
	if t.Status == store.CommentResolved {
		state = "✅ Çözüldü"
		if t.ResolvedBy != "" {
			state += " (" + plainLogin(t.ResolvedBy) + ")"
		}
	}
	fmt.Fprintf(&b, "### 💬 Preview yorumu: `%s` · %s\n\n", d.AppName, state)
	where := "`" + strings.ReplaceAll(Truncate(t.Path, 200), "`", "'") + "`"
	if t.ViewportWidth != nil {
		where += fmt.Sprintf(" · %d px genişlik", *t.ViewportWidth)
	}
	fmt.Fprintf(&b, "**%s** · %s · commit `%s`\n\n", plainLogin(t.AuthorLogin), where, naming.ShortSHA(d.CommitSHA))
	b.WriteString(quote(t.Body, t.Mentions) + "\n")
	if n := len(t.Replies); n > 0 {
		fmt.Fprintf(&b, "\n**Yanıtlar (%d)**\n\n", n)
		replies := t.Replies
		if n > maxSyncedReplies {
			fmt.Fprintf(&b, "_… önceki %d yanıt paas'ta_\n\n", n-maxSyncedReplies)
			replies = replies[n-maxSyncedReplies:]
		}
		for _, r := range replies {
			fmt.Fprintf(&b, "**%s:**\n%s\n\n", plainLogin(r.AuthorLogin), quote(Truncate(r.Body, 500), r.Mentions))
		}
	}
	if page := c.ReviewPage(d.ID, t.ID); page != "" {
		fmt.Fprintf(&b, "\n[paas'ta incele ve yanıtla →](%s)\n", page)
	}
	b.WriteString("\n<sub>Bu yorum, paas'taki yorum dizisi değiştikçe güncellenir.</sub>\n")
	return b.String()
}

var atRe = regexp.MustCompile(`@([A-Za-z0-9][A-Za-z0-9-]*)`)

// quote renders user text as a Markdown block quote: no HTML (so no fake
// markers), and only the validated mentions stay real @mentions; any other
// @word gets a zero-width space so GitHub does not notify that account.
func quote(s string, mentions []string) string {
	ok := map[string]bool{}
	for _, m := range mentions {
		ok[strings.ToLower(m)] = true
	}
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = atRe.ReplaceAllStringFunc(s, func(m string) string {
		if ok[strings.ToLower(m[1:])] {
			return m
		}
		return "@​" + m[1:]
	})
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// plainLogin names a user without notifying them on GitHub.
func plainLogin(login string) string {
	return "`" + strings.ReplaceAll(login, "`", "") + "`"
}

// DeleteMarkedComment deletes the comment on PR/issue number whose body
// contains marker; found is false when there is none.
func (c *Client) DeleteMarkedComment(ctx context.Context, repo string, number int, marker string) (bool, error) {
	if err := checkRepo(repo); err != nil {
		return false, err
	}
	if marker == "" {
		return false, errors.New("github: empty comment marker")
	}
	tok, err := c.repoToken(ctx, repo)
	if err != nil {
		return false, err
	}
	existing, err := c.findComment(ctx, tok, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, number), marker)
	if err != nil || existing == nil {
		return false, err
	}
	return true, c.do(ctx, tok, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, existing.ID), nil, nil)
}

func (c *CommentSync) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log
}
