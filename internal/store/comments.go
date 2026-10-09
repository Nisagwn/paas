package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"
)

// Faz 25: preview comments. A thread is pinned to a point of a deployment's
// page; replies belong to a thread. Both live in preview_comments.

// Thread statuses; they mirror the CHECK constraint.
const (
	CommentOpen     = "open"
	CommentResolved = "resolved"
)

// MaxCommentBody is the length limit of a comment, in characters.
const MaxCommentBody = 2000

// ErrNotThread: the comment is a reply, and the operation needs a thread.
var ErrNotThread = errors.New("comment is a reply, not a thread")

// Comment is a thread (ThreadID nil) or a reply.
type Comment struct {
	ID           int64     `json:"id"`
	DeploymentID int64     `json:"deployment_id"`
	AppID        int64     `json:"app_id"`
	ThreadID     *int64    `json:"thread_id,omitempty"`
	AuthorID     *int64    `json:"author_id,omitempty"`
	AuthorLogin  string    `json:"author_login"`
	Body         string    `json:"body"`
	Mentions     []string  `json:"mentions"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	// Threads only. Path is the page of the deployment ("/pricing"); X and
	// Y are percentages of the iframe viewport (nil: not pinned to a point);
	// ViewportWidth is the iframe width in CSS pixels (nil: unknown).
	Path          string     `json:"path,omitempty"`
	X             *float64   `json:"x,omitempty"`
	Y             *float64   `json:"y,omitempty"`
	ViewportWidth *int       `json:"viewport_width,omitempty"`
	Status        string     `json:"status,omitempty"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy    string     `json:"resolved_by,omitempty"`
	Replies       []Comment  `json:"replies,omitempty"`
}

// IsThread reports whether c is a thread rather than a reply.
func (c Comment) IsThread() bool { return c.ThreadID == nil }

// NewComment is a thread or reply to store. Author is nil for the admin.
type NewComment struct {
	AuthorID      *int64
	AuthorLogin   string
	Body          string
	Mentions      []string
	Path          string
	X, Y          *float64
	ViewportWidth *int
}

const commentCols = `id, deployment_id, app_id, thread_id, author_id, author_login, body, mentions,
	COALESCE(path, ''), x_pct, y_pct, viewport_width, COALESCE(status, ''), resolved_at, COALESCE(resolved_by, ''),
	created_at, updated_at`

func scanComment(row interface{ Scan(...any) error }) (Comment, error) {
	var c Comment
	var mentions pq.StringArray
	err := row.Scan(&c.ID, &c.DeploymentID, &c.AppID, &c.ThreadID, &c.AuthorID, &c.AuthorLogin, &c.Body, &mentions,
		&c.Path, &c.X, &c.Y, &c.ViewportWidth, &c.Status, &c.ResolvedAt, &c.ResolvedBy, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.Mentions = []string(mentions)
	if c.Mentions == nil {
		c.Mentions = []string{}
	}
	return c, err
}

// CreateThread opens a thread on deployment d.
func (s *Store) CreateThread(ctx context.Context, d Deployment, n NewComment) (Comment, error) {
	path := n.Path
	if path == "" {
		path = "/"
	}
	return scanComment(s.db.QueryRowContext(ctx, `
		INSERT INTO preview_comments (deployment_id, app_id, author_id, author_login, body, mentions,
			path, x_pct, y_pct, viewport_width, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'open')
		RETURNING `+commentCols,
		d.ID, d.AppID, n.AuthorID, n.AuthorLogin, n.Body, pq.StringArray(mentionsOrEmpty(n.Mentions)),
		path, n.X, n.Y, n.ViewportWidth))
}

// AddReply appends a reply to thread threadID. ErrNotFound: no such
// comment; ErrNotThread: threadID is itself a reply.
func (s *Store) AddReply(ctx context.Context, threadID int64, n NewComment) (Comment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Comment{}, err
	}
	defer tx.Rollback()
	t, err := scanComment(tx.QueryRowContext(ctx, `SELECT `+commentCols+` FROM preview_comments WHERE id = $1 FOR UPDATE`, threadID))
	if err != nil {
		return Comment{}, err
	}
	if !t.IsThread() {
		return Comment{}, ErrNotThread
	}
	c, err := scanComment(tx.QueryRowContext(ctx, `
		INSERT INTO preview_comments (deployment_id, app_id, thread_id, author_id, author_login, body, mentions)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+commentCols,
		t.DeploymentID, t.AppID, t.ID, n.AuthorID, n.AuthorLogin, n.Body, pq.StringArray(mentionsOrEmpty(n.Mentions))))
	if err != nil {
		return Comment{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE preview_comments SET updated_at = now() WHERE id = $1`, t.ID); err != nil {
		return Comment{}, err
	}
	return c, tx.Commit()
}

// GetComment returns a thread (with its replies) or a reply.
func (s *Store) GetComment(ctx context.Context, id int64) (Comment, error) {
	c, err := scanComment(s.db.QueryRowContext(ctx, `SELECT `+commentCols+` FROM preview_comments WHERE id = $1`, id))
	if err != nil || !c.IsThread() {
		return c, err
	}
	threads := []Comment{c}
	if err := s.attachReplies(ctx, threads); err != nil {
		return c, err
	}
	return threads[0], nil
}

// ListThreads returns the threads of a deployment with their replies,
// oldest first. status "" lists both open and resolved threads.
func (s *Store) ListThreads(ctx context.Context, deploymentID int64, status string) ([]Comment, error) {
	return s.listThreads(ctx, `deployment_id = $1`, deploymentID, status, 500)
}

// ListAppThreads returns the threads of every deployment of an app, newest
// first, at most limit.
func (s *Store) ListAppThreads(ctx context.Context, appID int64, status string, limit int) ([]Comment, error) {
	return s.listThreads(ctx, `app_id = $1`, appID, status, limit)
}

func (s *Store) listThreads(ctx context.Context, where string, arg any, status string, limit int) ([]Comment, error) {
	order := "id"
	if where == `app_id = $1` {
		order = "id DESC"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+commentCols+` FROM preview_comments
		WHERE `+where+` AND thread_id IS NULL AND ($2::text = '' OR status = $2::text)
		ORDER BY `+order+` LIMIT $3`, arg, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	threads := []Comment{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		threads = append(threads, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return threads, s.attachReplies(ctx, threads)
}

// attachReplies loads the replies of threads in one query.
func (s *Store) attachReplies(ctx context.Context, threads []Comment) error {
	if len(threads) == 0 {
		return nil
	}
	ids := make([]int64, len(threads))
	at := make(map[int64]int, len(threads))
	for i, t := range threads {
		ids[i], at[t.ID] = t.ID, i
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+commentCols+` FROM preview_comments
		WHERE thread_id = ANY($1) ORDER BY id`, pq.Int64Array(ids))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return err
		}
		i := at[*c.ThreadID]
		threads[i].Replies = append(threads[i].Replies, c)
	}
	return rows.Err()
}

// SetThreadStatus resolves or reopens a thread; by is the caller's login.
func (s *Store) SetThreadStatus(ctx context.Context, id int64, status, by string) (Comment, error) {
	if status != CommentOpen && status != CommentResolved {
		return Comment{}, errors.New("status must be open or resolved")
	}
	c, err := scanComment(s.db.QueryRowContext(ctx, `
		UPDATE preview_comments SET status = $2::text, updated_at = now(),
			resolved_at = CASE WHEN $2::text = 'resolved' THEN now() END,
			resolved_by = CASE WHEN $2::text = 'resolved' THEN $3::text END
		WHERE id = $1 AND thread_id IS NULL
		RETURNING `+commentCols, id, status, by))
	if errors.Is(err, ErrNotFound) {
		// A reply, or nothing at all.
		if _, gerr := s.GetComment(ctx, id); gerr == nil {
			return Comment{}, ErrNotThread
		}
	}
	if err != nil {
		return c, err
	}
	return s.GetComment(ctx, id)
}

// DeleteComment removes a comment; deleting a thread removes its replies.
func (s *Store) DeleteComment(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM preview_comments WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CommentCount is the number of threads of a deployment or an app.
type CommentCount struct {
	Open     int `json:"open"`
	Resolved int `json:"resolved"`
}

// OpenCommentCounts returns the number of open threads per deployment of
// an app; deployments without open threads are absent.
func (s *Store) OpenCommentCounts(ctx context.Context, appID int64) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT deployment_id, count(*) FROM preview_comments
		WHERE app_id = $1 AND thread_id IS NULL AND status = 'open' GROUP BY deployment_id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// CountThreads counts the threads of a deployment.
func (s *Store) CountThreads(ctx context.Context, deploymentID int64) (CommentCount, error) {
	return s.countThreads(ctx, `deployment_id = $1`, deploymentID)
}

// CountAppThreads counts the threads of every deployment of an app.
func (s *Store) CountAppThreads(ctx context.Context, appID int64) (CommentCount, error) {
	return s.countThreads(ctx, `app_id = $1`, appID)
}

func (s *Store) countThreads(ctx context.Context, where string, arg any) (CommentCount, error) {
	var c CommentCount
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE status = 'open'), count(*) FILTER (WHERE status = 'resolved')
		FROM preview_comments WHERE `+where+` AND thread_id IS NULL`, arg).Scan(&c.Open, &c.Resolved)
	return c, err
}

func mentionsOrEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
