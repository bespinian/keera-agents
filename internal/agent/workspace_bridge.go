package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/version"
)

// The bridge lets a coding agent in a workspace use the gateway's models and
// the workspace's tools without holding the gateway key: it listens on the
// loopback, takes any key, and adds the real one. A key copied out of the
// workspace would work anywhere; the bridge works only inside it.
//
//	/v1/...  the gateway's OpenAI-compatible API, as pi and Cline call it
//	/mcp     an MCP server: the platform tools and the gateway's MCP servers

func (r *runner) serveBridge(ctx context.Context) {
	ln, err := net.Listen("tcp", strings.TrimPrefix(BridgeURL, "http://"))
	if err != nil {
		r.log.Warn("workspace: the coding agent has no models, since the bridge could not listen", "error", err)
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", r.modelProxy())
	mux.HandleFunc("POST /mcp", r.serveMCP)
	// Streamable HTTP's other methods: no server-sent stream and no session
	// to end, which clients accept.
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("DELETE /mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	_ = srv.Serve(ln)
}

// modelProxy passes /v1/... to the gateway's API with the workspace's key.
// Answers stream through as they come. A 401 means the key was replaced, so
// the bootstrap is fetched again for the next request.
func (r *runner) modelProxy() http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			boot := r.snapshot()
			base, err := url.Parse(boot.Gateway.BaseURL)
			if err != nil {
				return
			}
			pr.Out.URL.Scheme, pr.Out.URL.Host = base.Scheme, base.Host
			pr.Out.URL.Path = strings.TrimRight(base.Path, "/") + strings.TrimPrefix(pr.In.URL.Path, "/v1")
			pr.Out.URL.RawPath = ""
			pr.Out.Host = base.Host
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Set("Authorization", "Bearer "+r.apiKey())
			// The gateway's Sessions screen shows the workspace as one session.
			pr.Out.Header.Set("X-Keera-Session", r.cfg.AgentID)
		},
		Transport:     r.gw.http.Transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusUnauthorized {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					_ = r.refresh(ctx)
				}()
			}
			return nil
		},
	}
}

// serveMCP answers one JSON-RPC message. Tools are the platform tools,
// which the control plane runs, and the gateway's MCP servers' tools, named
// alias__tool, which the bridge passes on with the key.
func (r *runner) serveMCP(w http.ResponseWriter, req *http.Request) {
	var msg rpcMessage
	if err := json.NewDecoder(io.LimitReader(req.Body, maxMCPBody)).Decode(&msg); err != nil {
		writeRPC(w, rpcMessage{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "not JSON"}})
		return
	}
	// A notification has no id and wants no answer.
	if len(msg.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	out := rpcMessage{JSONRPC: "2.0", ID: msg.ID}
	result, err := r.mcpMethod(req.Context(), msg)
	if err != nil {
		var re *rpcError
		if !errors.As(err, &re) {
			re = &rpcError{Code: -32603, Message: err.Error()}
		}
		out.Error = re
	} else {
		out.Result, _ = json.Marshal(result)
	}
	writeRPC(w, out)
}

func writeRPC(w http.ResponseWriter, msg rpcMessage) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}

func (r *runner) mcpMethod(ctx context.Context, msg rpcMessage) (any, error) {
	switch msg.Method {
	case "initialize":
		return map[string]any{
			// The version the runtime's own client speaks.
			"protocolVersion": mcpProtocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "keera", "version": version.String()},
			"instructions": "Tools of this Keera workspace: its connectors, such as repositories and " +
				"folders, and the organisation's MCP servers. They act for the workspace's owner.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": r.mcpTools()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		raw, _ := json.Marshal(msg.Params)
		if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" {
			return nil, &rpcError{Code: -32602, Message: "tools/call needs a name"}
		}
		text, isError := r.callBridgeTool(ctx, p.Name, p.Arguments)
		return map[string]any{
			"content": []map[string]string{{"type": "text", "text": text}},
			"isError": isError,
		}, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "no method " + msg.Method}
	}
}

type bridgeTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func (r *runner) mcpTools() []bridgeTool {
	boot := r.snapshot()
	out := []bridgeTool{}
	for _, t := range boot.Tools {
		out = append(out, bridgeTool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
	}
	for _, c := range r.mcpConns() {
		for _, t := range c.tools {
			out = append(out, bridgeTool{
				Name: mcpToolName(c.client.alias, t.Name), Description: t.Description, InputSchema: t.InputSchema,
			})
		}
	}
	for i := range out {
		if len(out[i].InputSchema) == 0 {
			out[i].InputSchema = json.RawMessage(`{"type":"object"}`)
		}
	}
	return out
}

// callBridgeTool runs a tool and says whether it failed.
func (r *runner) callBridgeTool(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	for _, t := range r.snapshot().Tools {
		if t.Name != name {
			continue
		}
		res, err := r.cp.workspaceTool(ctx, name, args)
		if err != nil {
			return err.Error(), true
		}
		return res.Content, res.IsError
	}
	for _, c := range r.mcpConns() {
		for _, t := range c.tools {
			if mcpToolName(c.client.alias, t.Name) != name {
				continue
			}
			text, err := c.client.call(ctx, r.cfg.AgentID, t.Name, args)
			if err != nil {
				return err.Error(), true
			}
			return text, false
		}
	}
	return "there is no tool called " + name, true
}

// workspaceTool runs a platform tool for the workspace, which has no task.
func (c *controlPlane) workspaceTool(ctx context.Context, name string, args json.RawMessage) (protocol.ToolResult, error) {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	var res protocol.ToolResult
	err := c.do(ctx, http.MethodPost, protocol.WorkspaceToolPath+"/"+url.PathEscape(name), args, &res)
	return res, err
}
