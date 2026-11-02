package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bespinian/keera-agents/internal/version"
)

// An MCP client for the Streamable HTTP transport: JSON-RPC 2.0 over POST,
// answered with JSON or an event stream. It does only what the runtime
// needs: initialize, list tools and call them. The gateway's MCP proxy
// stands between it and the server, and takes the agent's own key.

// mcpProtocol is the version asked for. A server answers with the one it
// speaks, which goes in Mcp-Protocol-Version from then on.
const mcpProtocol = "2025-06-18"

const (
	mcpConnectTimeout = 20 * time.Second
	mcpCallTimeout    = 5 * time.Minute
	maxMCPBody        = 16 << 20
	maxMCPResult      = 64 << 10
)

type mcpClient struct {
	alias string
	url   string
	http  *http.Client
	key   func() string

	mu       sync.Mutex
	session  string
	protocol string
	nextID   atomic.Int64
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (JSON-RPC %d)", e.Message, e.Code) }

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// connect initializes a session and lists the server's tools.
func (c *mcpClient) connect(ctx context.Context) ([]mcpTool, error) {
	ctx, cancel := context.WithTimeout(ctx, mcpConnectTimeout)
	defer cancel()
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	var tools []mcpTool
	cursor := ""
	for range 20 {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.request(ctx, "tools/list", params, "", false)
		if err != nil {
			return nil, fmt.Errorf("listing tools: %w", err)
		}
		var res struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("reading the tool list: %w", err)
		}
		tools = append(tools, res.Tools...)
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	return tools, nil
}

func (c *mcpClient) initialize(ctx context.Context) error {
	c.mu.Lock()
	c.session, c.protocol = "", ""
	c.mu.Unlock()
	raw, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "keera-agents", "version": version.String()},
	}, "", false)
	if err != nil {
		return fmt.Errorf("initializing: %w", err)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &res)
	c.mu.Lock()
	c.protocol = res.ProtocolVersion
	c.mu.Unlock()
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		return fmt.Errorf("initializing: %w", err)
	}
	return nil
}

// call runs a tool and turns its result into text for the model.
func (c *mcpClient) call(ctx context.Context, session, name string, args json.RawMessage) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	raw, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, session, true)
	if err != nil {
		return "", err
	}
	text, isError := toolResultText(raw)
	if isError {
		return "", errors.New(text)
	}
	return text, nil
}

// toolResultText turns a result into text, and says whether the server
// reported it as an error.
func toolResultText(raw json.RawMessage) (string, bool) {
	var res struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			Resource struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"resource"`
			URI string `json:"uri"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return truncate(string(raw), maxMCPResult), false
	}
	var b strings.Builder
	for _, c := range res.Content {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		switch c.Type {
		case "text":
			b.WriteString(c.Text)
		case "resource":
			if c.Resource.Text != "" {
				b.WriteString(c.Resource.Text)
			} else {
				fmt.Fprintf(&b, "[resource %s]", c.Resource.URI)
			}
		case "resource_link":
			fmt.Fprintf(&b, "[link %s]", c.URI)
		default:
			fmt.Fprintf(&b, "[%s %s omitted]", c.Type, c.MimeType)
		}
	}
	if b.Len() == 0 && len(res.StructuredContent) > 0 {
		b.Write(res.StructuredContent)
	}
	return truncate(b.String(), maxMCPResult), res.IsError
}

func (c *mcpClient) notify(ctx context.Context, method string) error {
	_, err := c.post(ctx, rpcMessage{JSONRPC: "2.0", Method: method}, "", false)
	return err
}

// request sends one request and waits for its answer. A session the server
// forgot (404) is initialized again, once.
func (c *mcpClient) request(ctx context.Context, method string, params any, session string, retry bool) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	msg := rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(id)), Method: method, Params: params}
	c.mu.Lock()
	hadSession := c.session != ""
	c.mu.Unlock()
	res, err := c.post(ctx, msg, session, true)
	if retry && hadSession && statusOf(err) == http.StatusNotFound {
		if err := c.initialize(ctx); err != nil {
			return nil, err
		}
		return c.request(ctx, method, params, session, false)
	}
	if err != nil {
		return nil, err
	}
	if res.Error != nil {
		return nil, res.Error
	}
	return res.Result, nil
}

// post sends a message. With wantReply it reads the answer with the same id
// from a JSON body or an event stream; otherwise it expects 202.
func (c *mcpClient) post(ctx context.Context, msg rpcMessage, session string, wantReply bool) (rpcMessage, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return rpcMessage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return rpcMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.key())
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Mcp-Method", msg.Method)
	if p, ok := msg.Params.(map[string]any); ok && msg.Method == "tools/call" {
		if name, _ := p["name"].(string); plainHeader(name) {
			req.Header.Set("Mcp-Name", name)
		}
	}
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}
	c.mu.Lock()
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.protocol != "" {
		req.Header.Set("Mcp-Protocol-Version", c.protocol)
	}
	c.mu.Unlock()

	resp, err := c.http.Do(req)
	if err != nil {
		return rpcMessage{}, fmt.Errorf("reaching MCP server %s: %w", c.alias, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		c.mu.Lock()
		c.session = id
		c.mu.Unlock()
	}
	if resp.StatusCode >= 300 {
		return rpcMessage{}, readError("MCP server "+c.alias, resp)
	}
	if !wantReply {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return rpcMessage{}, nil
	}

	ctype := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ctype, "text/event-stream") {
		return readSSEReply(resp.Body, msg.ID)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPBody))
	if err != nil {
		return rpcMessage{}, err
	}
	if reply, ok := findReply(raw, msg.ID); ok {
		return reply, nil
	}
	return rpcMessage{}, fmt.Errorf("MCP server %s gave no answer to %s", c.alias, msg.Method)
}

// findReply picks the response with id out of one message or a batch.
func findReply(raw []byte, id json.RawMessage) (rpcMessage, bool) {
	raw = bytes.TrimSpace(raw)
	var msgs []rpcMessage
	if len(raw) > 0 && raw[0] == '[' {
		if json.Unmarshal(raw, &msgs) != nil {
			return rpcMessage{}, false
		}
	} else {
		var m rpcMessage
		if json.Unmarshal(raw, &m) != nil {
			return rpcMessage{}, false
		}
		msgs = []rpcMessage{m}
	}
	for _, m := range msgs {
		if m.Method == "" && bytes.Equal(bytes.TrimSpace(m.ID), id) {
			return m, true
		}
	}
	return rpcMessage{}, false
}

// readSSEReply reads events until the response to id. The server's own
// notifications and requests on the way are skipped: the runtime offers no
// sampling or elicitation, so it said none in initialize.
func readSSEReply(r io.Reader, id json.RawMessage) (rpcMessage, error) {
	sc := bufio.NewScanner(io.LimitReader(r, maxMCPBody))
	sc.Buffer(make([]byte, 64<<10), maxMCPBody)
	var data strings.Builder
	dispatch := func() (rpcMessage, bool) {
		defer data.Reset()
		if data.Len() == 0 {
			return rpcMessage{}, false
		}
		return findReply([]byte(data.String()), id)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if m, ok := dispatch(); ok {
				return m, nil
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if m, ok := dispatch(); ok {
		return m, nil
	}
	if err := sc.Err(); err != nil {
		return rpcMessage{}, fmt.Errorf("reading the event stream: %w", err)
	}
	return rpcMessage{}, errors.New("the event stream ended without an answer")
}

func plainHeader(v string) bool {
	if v == "" || v != strings.TrimSpace(v) {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

var reToolName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// mcpToolName is how the model sees a server's tool: alias__tool, within
// what OpenAI allows for a function name.
func mcpToolName(alias, name string) string {
	s := reToolName.ReplaceAllString(alias, "_") + "__" + reToolName.ReplaceAllString(name, "_")
	if len(s) <= 64 {
		return s
	}
	sum := sha256.Sum256([]byte(alias + "/" + name))
	return s[:55] + "_" + hex.EncodeToString(sum[:4])
}
