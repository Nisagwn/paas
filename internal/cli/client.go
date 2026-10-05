package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// requestTimeout bounds every non-streaming API call.
const requestTimeout = 60 * time.Second

// Client calls the paas HTTP API with a bearer token.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// APIError is a non-2xx answer from the API, with a hint for the user.
type APIError struct {
	Status  int
	Message string
	hint    string
}

func (e *APIError) Error() string { return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status) }

// Hint is printed under the error message.
func (e *APIError) Hint() string { return e.hint }

// hintError is a local error with a hint (not logged in, unreachable host).
type hintError struct {
	msg, hint string
	err       error
}

func (e *hintError) Error() string { return e.msg }
func (e *hintError) Hint() string  { return e.hint }
func (e *hintError) Unwrap() error { return e.err }

func newAPIError(status int, body []byte, base string) *APIError {
	var payload struct {
		Error string `json:"error"`
	}
	msg := ""
	if json.Unmarshal(body, &payload) == nil {
		msg = payload.Error
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
		if len(msg) > 200 || strings.HasPrefix(msg, "<") {
			msg = ""
		}
	}
	if msg == "" {
		msg = strings.ToLower(http.StatusText(status))
	}
	e := &APIError{Status: status, Message: msg}
	switch {
	case status == http.StatusUnauthorized:
		e.hint = "The token is missing, expired or revoked. Create a new one at " + base +
			"/tokens and run `paas login`, or set PAAS_TOKEN."
	case status == http.StatusForbidden:
		e.hint = "Your role on the app's team does not allow this: reads need viewer, changes need member, " +
			"team management needs owner. Ask a team owner to change your role."
	case status == http.StatusNotFound:
		e.hint = "Check the app name with `paas ls` and deployment ids with `paas deployments <app>`. " +
			"Apps of teams you are not a member of also answer 404."
	case status == http.StatusNotImplemented:
		e.hint = "This feature is not enabled on the server."
	case status >= 500:
		e.hint = "The server could not complete the request; try again, or check the platform logs."
	}
	return e
}

func (c *Client) request(ctx context.Context, method, path string, in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "paas-cli")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) netError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &hintError{
		msg:  fmt.Sprintf("cannot reach %s: %v", c.BaseURL, err),
		hint: "Check the platform URL (--url, PAAS_URL or `paas login --url`) and your network.",
		err:  err,
	}
}

// Do sends in as JSON (nil: no body), decodes the answer into out (nil:
// ignored) and returns the raw body for --json output.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := c.request(ctx, method, path, in)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, c.netError(ctx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, c.netError(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, newAPIError(resp.StatusCode, data, c.BaseURL)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, &hintError{
				msg:  fmt.Sprintf("unexpected response from %s%s", c.BaseURL, path),
				hint: "Is the platform URL right? It must point at the paas control plane, not an app.",
				err:  err,
			}
		}
	}
	return data, nil
}

// Stream opens a long-lived GET (SSE or chunked text); the caller closes
// the body. Non-2xx answers become an *APIError.
func (c *Client) Stream(ctx context.Context, path, accept string) (*http.Response, error) {
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, c.netError(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, newAPIError(resp.StatusCode, data, c.BaseURL)
	}
	return resp, nil
}

// sseEvent is one Server-Sent Event.
type sseEvent struct {
	ID    string
	Event string
	Data  string
}

// readSSE parses an event stream and calls fn for every event until fn
// returns stop, the stream ends (nil) or reading fails.
func readSSE(r io.Reader, fn func(sseEvent) (stop bool, err error)) error {
	br := bufio.NewReader(r)
	var (
		ev      sseEvent
		data    []string
		hasData bool
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil && line == "" {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if hasData || ev.Event != "" {
				ev.Data = strings.Join(data, "\n")
				stop, ferr := fn(ev)
				if ferr != nil || stop {
					return ferr
				}
			}
			ev, data, hasData = sseEvent{}, nil, false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment (heartbeat)
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			ev.ID = value
		case "event":
			ev.Event = value
		case "data":
			data = append(data, value)
			hasData = true
		}
		if err != nil { // last line without a newline
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
