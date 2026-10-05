package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// logPage matches the API's page size for /logs?after=.
const logPage = 1000

// maxStreamRetries: consecutive reconnects without a new log line or
// status before `logs -f` gives up.
const maxStreamRetries = 5

func cmdLogs(r *runner, args []string) error {
	fs := r.flags()
	follow := fs.Bool("f", false, "follow: stream new lines until the deployment finishes (runtime: until the pod stops)")
	fs.BoolVar(follow, "follow", false, "same as -f")
	runtimeLogs := fs.Bool("runtime", false, "pod logs of a running deployment: paas logs --runtime <app> <deployment-id>")
	tail := fs.Int("tail", 200, "runtime logs: number of recent lines (1-10000)")
	pos, err := r.parse(fs, args, 1, 2)
	if err != nil {
		return err
	}
	if *runtimeLogs {
		if len(pos) != 2 {
			return usagef("--runtime needs <app> <deployment-id>")
		}
		if *tail < 1 || *tail > 10000 {
			return usagef("--tail must be between 1 and 10000")
		}
		id, err := parseID(pos[1])
		if err != nil {
			return err
		}
		return r.runtimeLogs(pos[0], id, *follow, *tail)
	}
	if len(pos) != 1 {
		return usagef("build logs take one <deployment-id>; pod logs need --runtime <app> <deployment-id>")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	c, err := r.client()
	if err != nil {
		return err
	}
	if *follow {
		return r.followLogs(c, id)
	}
	return r.printLogs(c, id)
}

// printLogs pages through everything written so far.
func (r *runner) printLogs(c *Client, id int64) error {
	var after int64
	all := []logLine{}
	for {
		var page []logLine
		path := fmt.Sprintf("/api/deployments/%d/logs?after=%d", id, after)
		if _, err := c.Do(r.ctx, "GET", path, nil, &page); err != nil {
			return err
		}
		for _, l := range page {
			if !r.g.JSON {
				fmt.Fprintln(r.Stdout, l.Line)
			}
			after = l.ID
		}
		all = append(all, page...)
		if len(page) < logPage {
			break
		}
	}
	if r.g.JSON {
		return writeJSON(r.Stdout, all)
	}
	if len(all) == 0 {
		fmt.Fprintf(r.Stderr, "No log lines yet. Follow the build with: paas logs %d -f\n", id)
	}
	return nil
}

type statusEvent struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Final  bool   `json:"final"`
}

// deploymentFailed: a deployment finished as failed (exit 1, message shown).
type deploymentFailed struct {
	id  int64
	msg string
}

func (e *deploymentFailed) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("deployment #%d failed", e.id)
	}
	return fmt.Sprintf("deployment #%d failed: %s", e.id, e.msg)
}

// followLogs reads the SSE stream until its final status event,
// reconnecting after the last line seen when the connection drops.
func (r *runner) followLogs(c *Client, id int64) error {
	var after int64
	retries := 0
	for {
		final, progressed, err := r.streamOnce(c, id, &after)
		if final != nil {
			if r.g.JSON {
				writeLine(r.Stdout, map[string]any{"event": "status", "status": final.Status, "error": final.Error, "final": true})
			}
			if final.Status == "ready" {
				if !r.g.JSON {
					fmt.Fprintf(r.Stderr, "Deployment #%d is ready.\n", id)
				}
				return nil
			}
			if final.Status == "failed" {
				return &deploymentFailed{id: id, msg: final.Error}
			}
			if !r.g.JSON {
				fmt.Fprintf(r.Stderr, "Deployment #%d finished: %s.\n", id, final.Status)
			}
			return nil
		}
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return err
		}
		if progressed {
			retries = 0
		}
		retries++
		if retries > maxStreamRetries {
			if err == nil {
				err = errors.New("the server closed the stream")
			}
			return fmt.Errorf("log stream for deployment #%d keeps dropping: %w", id, err)
		}
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(r.ReconnectDelay):
		}
	}
}

// streamOnce reads one connection of the stream; final is set when the
// server sent the closing status event.
func (r *runner) streamOnce(c *Client, id int64, after *int64) (final *statusEvent, progressed bool, err error) {
	path := fmt.Sprintf("/api/deployments/%d/logs/stream?after=%d", id, *after)
	resp, err := c.Stream(r.ctx, path, "text/event-stream")
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	err = readSSE(resp.Body, func(ev sseEvent) (bool, error) {
		switch ev.Event {
		case "", "message":
			if n, perr := strconv.ParseInt(ev.ID, 10, 64); perr == nil && n > *after {
				*after = n
			}
			progressed = true
			if r.g.JSON {
				n, _ := strconv.ParseInt(ev.ID, 10, 64)
				return false, writeLine(r.Stdout, map[string]any{"id": n, "line": ev.Data})
			}
			_, werr := fmt.Fprintln(r.Stdout, ev.Data)
			return false, werr
		case "status":
			var st statusEvent
			if json.Unmarshal([]byte(ev.Data), &st) != nil {
				return false, nil
			}
			progressed = true
			if st.Final {
				final = &st
				return true, nil
			}
			if r.g.JSON {
				return false, writeLine(r.Stdout, map[string]any{"event": "status", "status": st.Status, "final": false})
			}
			fmt.Fprintf(r.Stderr, "[paas] deployment #%d is %s\n", id, st.Status)
		}
		return false, nil
	})
	return final, progressed, err
}

// writeLine prints v as one compact JSON line (NDJSON for streams).
func writeLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// runtimeLogs copies the pod's log (chunked text/plain) to stdout.
func (r *runner) runtimeLogs(appName string, id int64, follow bool, tail int) error {
	c, err := r.client()
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s?tail=%d", appPath(appName, "deployments", strconv.FormatInt(id, 10), "runtime-logs"), tail)
	if follow {
		path += "&follow=1"
	}
	resp, err := c.Stream(r.ctx, path, "text/plain")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(r.Stdout, resp.Body); err != nil && r.ctx.Err() == nil {
		return fmt.Errorf("reading runtime logs: %w", err)
	}
	if r.ctx.Err() != nil {
		return r.ctx.Err()
	}
	return nil
}
