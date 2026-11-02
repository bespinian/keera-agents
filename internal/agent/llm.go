package agent

import (
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

// The model is reached through the gateway's OpenAI-compatible Chat
// Completions API, without streaming: nobody watches the tokens arrive, and
// the panel shows whole steps.

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"-"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// MarshalJSON sends an assistant's empty content as null: some providers
// refuse an empty text next to tool calls.
func (m message) MarshalJSON() ([]byte, error) {
	type plain message
	var content any = m.Content
	if m.Content == "" && len(m.ToolCalls) > 0 {
		content = nil
	}
	return json.Marshal(struct {
		plain
		Content any `json:"content"`
	}{plain(m), content})
}

// UnmarshalJSON takes content as a string or as a list of parts, since not
// every backend behind the gateway answers with a string.
func (m *message) UnmarshalJSON(raw []byte) error {
	type plain message
	var v struct {
		plain
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*m = message(v.plain)
	m.Content = contentText(v.Content)
	return nil
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolDef struct {
	Type     string      `json:"type"`
	Function functionDef `json:"function"`
}

type functionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []toolDef `json:"tools,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
}

type gateway struct {
	http *http.Client
	// retryWait is the first wait after a refusal worth retrying; tests
	// shorten it.
	retryWait time.Duration
}

// completionTimeout bounds one model call. A long answer without streaming
// takes minutes on a slow model.
const completionTimeout = 10 * time.Minute

// complete makes one model call. It retries a rate limit, an overloaded
// backend and a dropped connection a few times, and returns any other
// refusal as an *httpError carrying the gateway's own message.
func (g *gateway) complete(ctx context.Context, baseURL, apiKey, session string, req chatRequest) (message, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return message{}, err
	}
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	wait := g.retryWait
	for attempt := 0; ; attempt++ {
		msg, err := g.post(ctx, url, apiKey, session, raw)
		if err == nil || attempt >= 3 || ctx.Err() != nil || !retryable(err) {
			return msg, err
		}
		d := wait
		if he, ok := errors.AsType[*httpError](err); ok && he.RetryAfter > 0 {
			d = min(he.RetryAfter, 60*time.Second)
		}
		select {
		case <-ctx.Done():
			return message{}, err
		case <-time.After(d):
		}
		wait *= 2
	}
}

func retryable(err error) bool {
	switch statusOf(err) {
	case 0:
		// Not an answer at all: the connection failed.
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (g *gateway) post(ctx context.Context, url, apiKey, session string, body []byte) (message, error) {
	ctx, cancel := context.WithTimeout(ctx, completionTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return message{}, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return message{}, fmt.Errorf("reaching the gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return message{}, readError("the gateway", resp)
	}
	var out chatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return message{}, fmt.Errorf("reading the gateway's answer: %w", err)
	}
	if len(out.Choices) == 0 {
		return message{}, errors.New("the gateway answered without a choice")
	}
	msg := out.Choices[0].Message
	msg.Role = "assistant"
	return msg, nil
}

// sessionHeader carries the task's id on every gateway request, so the
// gateway shows each task as one session.
const sessionHeader = "X-Keera-Session"

// contextTooLong reports whether the model refused the request for its size.
func contextTooLong(err error) bool {
	he, ok := errors.AsType[*httpError](err)
	if !ok {
		return false
	}
	return he.Code == "context_length_exceeded" ||
		(he.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(he.Msg), "context length"))
}
