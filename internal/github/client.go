// Package github is a REST client for the few GitHub API calls paas makes:
// commit statuses, open pull requests of a branch, PR comments and, as a
// GitHub App (Faz 15), installation tokens. It uses net/http only.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultBaseURL is the public GitHub API. GitHub Enterprise Server uses
// https://<host>/api/v3.
const DefaultBaseURL = "https://api.github.com"

// Commit status states.
const (
	StatePending = "pending"
	StateSuccess = "success"
	StateFailure = "failure"
	StateError   = "error"
)

// MaxDescription is GitHub's limit for a commit status description.
const MaxDescription = 140

// lowRateLimit is the remaining-requests level below which every response
// is logged as a warning.
const lowRateLimit = 100

type Client struct {
	// BaseURL without a trailing slash, e.g. https://api.github.com.
	BaseURL string
	Token   string
	// Tokens, when set, replaces Token: repository calls authenticate with
	// the token it returns for that repository (Faz 15, GitHub App).
	Tokens    TokenSource
	UserAgent string
	HTTP      *http.Client
	Log       *slog.Logger
}

// New returns a client with a 10 s request timeout.
func New(baseURL, token string, log *slog.Logger) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Token:     token,
		UserAgent: "paas",
		HTTP:      &http.Client{Timeout: 10 * time.Second},
		Log:       log,
	}
}

// Error is a non-2xx response from the API.
type Error struct {
	Method     string
	Path       string
	StatusCode int
	// Message is GitHub's "message" field (plus validation details), or the
	// start of the body when it is not JSON.
	Message string
	// RetryAfter is set when GitHub rate-limited the request.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	s := fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.StatusCode, e.Message)
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(" (rate limited, retry in %s)", e.RetryAfter.Round(time.Second))
	}
	return s
}

// Status is a commit status. Context defaults to "paas/deploy".
type Status struct {
	State       string `json:"state"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Context     string `json:"context"`
}

// CreateCommitStatus sets a status on a commit of repo ("owner/name").
// The description is truncated to GitHub's 140-character limit.
func (c *Client) CreateCommitStatus(ctx context.Context, repo, sha string, s Status) error {
	if err := checkRepo(repo); err != nil {
		return err
	}
	switch s.State {
	case StatePending, StateSuccess, StateFailure, StateError:
	default:
		return fmt.Errorf("github: invalid commit status state %q", s.State)
	}
	if s.Context == "" {
		s.Context = "paas/deploy"
	}
	s.Description = Truncate(s.Description, MaxDescription)
	tok, err := c.repoToken(ctx, repo)
	if err != nil {
		return err
	}
	return c.do(ctx, tok, http.MethodPost, "/repos/"+repo+"/statuses/"+url.PathEscape(sha), s, nil)
}

type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// OpenPullRequests returns the open pull requests whose head is branch in
// repo itself. PRs from forks have the fork owner in their head and are not
// found; they never trigger a deployment anyway, because pushes to a fork
// do not reach this repository's webhook.
func (c *Client) OpenPullRequests(ctx context.Context, repo, branch string) ([]PullRequest, error) {
	if err := checkRepo(repo); err != nil {
		return nil, err
	}
	owner, _, _ := strings.Cut(repo, "/")
	q := url.Values{"state": {"open"}, "head": {owner + ":" + branch}, "per_page": {"100"}}
	tok, err := c.repoToken(ctx, repo)
	if err != nil {
		return nil, err
	}
	var prs []PullRequest
	err = c.do(ctx, tok, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, &prs)
	return prs, err
}

type Comment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

// maxCommentPages bounds the search for our comment on very long threads.
const maxCommentPages = 10

// UpsertComment updates the comment on PR/issue number whose body contains
// marker (a hidden HTML comment such as "<!-- paas:preview:blog -->"), or
// creates one. body must contain marker so the next call finds it again.
func (c *Client) UpsertComment(ctx context.Context, repo string, number int, marker, body string) (Comment, bool, error) {
	if err := checkRepo(repo); err != nil {
		return Comment{}, false, err
	}
	if marker == "" || !strings.Contains(body, marker) {
		return Comment{}, false, errors.New("github: comment body must contain the marker")
	}
	tok, err := c.repoToken(ctx, repo)
	if err != nil {
		return Comment{}, false, err
	}
	base := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number)
	existing, err := c.findComment(ctx, tok, base+"?per_page=100", marker)
	if err != nil {
		return Comment{}, false, err
	}
	payload := map[string]string{"body": body}
	var out Comment
	if existing != nil {
		err = c.do(ctx, tok, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, existing.ID), payload, &out)
		return out, false, err
	}
	err = c.do(ctx, tok, http.MethodPost, base, payload, &out)
	return out, true, err
}

func (c *Client) findComment(ctx context.Context, token, path, marker string) (*Comment, error) {
	for page := 0; page < maxCommentPages && path != ""; page++ {
		var comments []Comment
		next, err := c.request(ctx, token, http.MethodGet, path, nil, &comments)
		if err != nil {
			return nil, err
		}
		for i := range comments {
			if strings.Contains(comments[i].Body, marker) {
				return &comments[i], nil
			}
		}
		path = next
	}
	return nil, nil
}

// repoToken is the token for a call about repo: from Tokens when set
// (ErrNoCredentials if it has none), else the static Token.
func (c *Client) repoToken(ctx context.Context, repo string) (string, error) {
	if c.Tokens == nil {
		return c.Token, nil
	}
	tok, err := c.Tokens.RepoToken(ctx, repo)
	if err != nil {
		return "", err
	}
	if tok == "" {
		return "", fmt.Errorf("%w %s", ErrNoCredentials, repo)
	}
	return tok, nil
}

func (c *Client) do(ctx context.Context, token, method, path string, in, out any) error {
	_, err := c.request(ctx, token, method, path, in, out)
	return err
}

// request sends one API call authenticated with token (none when empty) and
// decodes the JSON response into out. It returns the path of the next page
// from the Link header, if any.
func (c *Client) request(ctx context.Context, token, method, path string, in, out any) (string, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return "", err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.UserAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: %s %s: %w", method, stripQuery(path), err)
	}
	defer resp.Body.Close()
	c.logRateLimit(resp, method, path)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", c.apiError(resp, method, path)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(out); err != nil {
			return "", fmt.Errorf("github: %s %s: decode response: %w", method, stripQuery(path), err)
		}
	}
	return c.nextPage(resp.Header.Get("Link")), nil
}

func (c *Client) apiError(resp *http.Response, method, path string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Method: method, Path: stripQuery(path), StatusCode: resp.StatusCode}

	var body struct {
		Message string `json:"message"`
		Errors  []struct {
			Resource string `json:"resource"`
			Field    string `json:"field"`
			Code     string `json:"code"`
			Message  string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Message != "" {
		e.Message = body.Message
		var details []string
		for _, d := range body.Errors {
			switch {
			case d.Message != "":
				details = append(details, d.Message)
			case d.Field != "":
				details = append(details, d.Field+" "+d.Code)
			case d.Code != "":
				details = append(details, d.Code)
			}
		}
		if len(details) > 0 {
			e.Message += ": " + strings.Join(details, "; ")
		}
	} else {
		e.Message = Truncate(strings.TrimSpace(string(raw)), 200)
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
	}

	// Primary limit: 403/429 with X-RateLimit-Remaining 0. Secondary limit:
	// Retry-After in seconds.
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			e.RetryAfter = time.Duration(s) * time.Second
		} else if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				e.RetryAfter = max(time.Until(time.Unix(reset, 0)), time.Second)
			}
		}
	}
	return e
}

func (c *Client) logRateLimit(resp *http.Response, method, path string) {
	if c.Log == nil {
		return
	}
	remaining, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	if err != nil || remaining >= lowRateLimit {
		return
	}
	attrs := []any{"method", method, "path", stripQuery(path), "remaining", remaining,
		"limit", resp.Header.Get("X-RateLimit-Limit")}
	if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		attrs = append(attrs, "reset", time.Unix(reset, 0).UTC().Format(time.RFC3339))
	}
	c.Log.Warn("github rate limit low", attrs...)
}

// nextPage extracts rel="next" from a Link header. Only URLs under BaseURL
// are followed, so the token is never sent to another host.
func (c *Client) nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		target = strings.Trim(strings.TrimSpace(target), "<>")
		if rest, ok := strings.CutPrefix(target, c.BaseURL+"/"); ok {
			return "/" + rest
		}
	}
	return ""
}

func checkRepo(repo string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(name, "/?#") {
		return fmt.Errorf("github: repo must look like \"owner/name\", got %q", repo)
	}
	return nil
}

func stripQuery(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}

// Truncate shortens s to at most n characters (runes), ending with "…".
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 1 {
		return string([]rune(s)[:n])
	}
	return strings.TrimRight(string([]rune(s)[:n-1]), " ") + "…"
}
