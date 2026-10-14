package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
)

// controlPlane calls the runtime API. The runtime token is its only
// credential and goes nowhere else.
type controlPlane struct {
	base  string
	token string
	http  *http.Client
}

// httpError is an answer with a status of 300 or more. Msg is the body's
// message, never the request, so it holds no credential.
type httpError struct {
	Status int
	Msg    string
	// Code is an OpenAI-shaped error's code, such as context_length_exceeded.
	Code string
	// RetryAfter is the server's Retry-After, if any.
	RetryAfter time.Duration
	what       string
}

func (e *httpError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("%s: %s (%d)", e.what, e.Msg, e.Status)
	}
	return fmt.Sprintf("%s answered %d %s", e.what, e.Status, http.StatusText(e.Status))
}

func statusOf(err error) int {
	if he, ok := errors.AsType[*httpError](err); ok {
		return he.Status
	}
	return 0
}

// readError reads an error body in whichever shape it comes: OpenAI's
// {"error":{"message"}}, {"error":"..."}, {"message":"..."}, or plain text.
func readError(what string, resp *http.Response) *httpError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &httpError{Status: resp.StatusCode, what: what}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := time.ParseDuration(s + "s"); err == nil {
			e.RetryAfter = secs
		}
	}
	var body struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &body) == nil {
		var inner struct {
			Message string          `json:"message"`
			Code    json.RawMessage `json:"code"`
		}
		var s string
		switch {
		case json.Unmarshal(body.Error, &inner) == nil && inner.Message != "":
			e.Msg = inner.Message
			e.Code = strings.Trim(string(inner.Code), `"`)
			if e.Code == "null" {
				e.Code = ""
			}
		case json.Unmarshal(body.Error, &s) == nil && s != "":
			e.Msg = s
		case body.Message != "":
			e.Msg = body.Message
		}
	}
	if e.Msg == "" && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		e.Msg = truncate(strings.TrimSpace(string(raw)), 300)
	}
	return e
}

// do calls the runtime API. A 204 leaves out as it was.
func (c *controlPlane) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching keera-agents: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return readError("keera-agents", resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

func (c *controlPlane) bootstrap(ctx context.Context) (protocol.Bootstrap, error) {
	var b protocol.Bootstrap
	err := c.do(ctx, http.MethodGet, protocol.BootstrapPath, nil, &b)
	return b, err
}

// next waits up to wait for a task. No task is nil and no error.
func (c *controlPlane) next(ctx context.Context, wait time.Duration) (*protocol.Task, error) {
	// Longer than the wait, so the server answers before the client gives up.
	ctx, cancel := context.WithTimeout(ctx, wait+20*time.Second)
	defer cancel()
	// A 204 leaves t empty, which is no task.
	var t protocol.Task
	err := c.do(ctx, http.MethodGet, protocol.NextTaskPath+"?wait="+url.QueryEscape(wait.String()), nil, &t)
	if err != nil || t.ID == "" {
		return nil, err
	}
	return &t, nil
}

func (c *controlPlane) events(ctx context.Context, taskID string, events []protocol.Event) error {
	return c.do(ctx, http.MethodPost, taskPath(taskID, "events"), protocol.Events{Events: events}, nil)
}

func (c *controlPlane) finish(ctx context.Context, taskID string, f protocol.Finish) error {
	return c.do(ctx, http.MethodPost, taskPath(taskID, "finish"), f, nil)
}

// tool runs a platform tool at tools, a task's route or the workspace's. The
// body is the tool's arguments.
func (c *controlPlane) tool(ctx context.Context, tools, name string, args json.RawMessage) (protocol.ToolResult, error) {
	var r protocol.ToolResult
	err := c.do(ctx, http.MethodPost, tools+"/"+url.PathEscape(name), args, &r)
	return r, err
}

func taskPath(id, what string) string {
	return protocol.TaskPath + "/" + url.PathEscape(id) + "/" + what
}
