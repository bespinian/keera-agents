package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
)

// The runtime against stand-ins for the control plane, the gateway and an
// MCP server.

const runtimeToken = "rt_secret_token"

type fakeControlPlane struct {
	srv *httptest.Server

	mu         sync.Mutex
	boot       protocol.Bootstrap
	bootstraps int
	tasks      []protocol.Task
	events     []protocol.Event
	toolCalls  []platformCall
	finished   chan protocol.Finish
	// finishFails is how many finishes to refuse with 503 first.
	finishFails int
}

func newFakeControlPlane(t *testing.T) *fakeControlPlane {
	f := &fakeControlPlane{finished: make(chan protocol.Finish, 10)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+runtimeToken {
			http.Error(w, `{"error":{"message":"bad token"}}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == protocol.BootstrapPath:
			f.bootstraps++
			_ = json.NewEncoder(w).Encode(f.boot)
		case r.URL.Path == protocol.NextTaskPath:
			if r.URL.Query().Get("wait") == "" {
				t.Errorf("next task without a wait")
			}
			if len(f.tasks) == 0 {
				f.mu.Unlock()
				time.Sleep(10 * time.Millisecond)
				f.mu.Lock()
				w.WriteHeader(http.StatusNoContent)
				return
			}
			task := f.tasks[0]
			f.tasks = f.tasks[1:]
			_ = json.NewEncoder(w).Encode(task)
		case strings.HasSuffix(r.URL.Path, "/events"):
			var evs protocol.Events
			_ = json.NewDecoder(r.Body).Decode(&evs)
			f.events = append(f.events, evs.Events...)
		case strings.HasSuffix(r.URL.Path, "/finish"):
			if f.finishFails > 0 {
				f.finishFails--
				http.Error(w, `{"error":{"message":"down"}}`, http.StatusServiceUnavailable)
				return
			}
			var fin protocol.Finish
			_ = json.NewDecoder(r.Body).Decode(&fin)
			f.finished <- fin
		case strings.Contains(r.URL.Path, "/tools/"):
			call := platformCall{Path: r.URL.Path}
			_ = json.NewDecoder(r.Body).Decode(&call.Args)
			f.toolCalls = append(f.toolCalls, call)
			_ = json.NewEncoder(w).Encode(protocol.ToolResult{Content: "posted to #general"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type platformCall struct {
	Path string
	Args map[string]any
}

func (f *fakeControlPlane) push(task protocol.Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks = append(f.tasks, task)
}

func (f *fakeControlPlane) setBoot(fn func(b *protocol.Bootstrap)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.boot)
}

func (f *fakeControlPlane) wait(t *testing.T) protocol.Finish {
	t.Helper()
	select {
	case fin := <-f.finished:
		return fin
	case <-time.After(10 * time.Second):
		t.Fatal("the task did not finish")
		return protocol.Finish{}
	}
}

func (f *fakeControlPlane) allEvents() []protocol.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Event(nil), f.events...)
}

// fakeGateway answers chat completions with what reply returns for the n-th
// call (from 0), and records every request.
type fakeGateway struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []gwRequest
	reply func(n int, req gwRequest) (status int, body string)
}

type gwRequest struct {
	Auth    string
	Session string
	Body    chatRequest
	Raw     string
}

func newFakeGateway(t *testing.T, reply func(n int, req gwRequest) (int, string)) *fakeGateway {
	g := &fakeGateway{reply: reply}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		req := gwRequest{Auth: r.Header.Get("Authorization"), Session: r.Header.Get("X-Keera-Session"), Raw: string(raw)}
		if err := json.Unmarshal(raw, &req.Body); err != nil {
			t.Errorf("the gateway got a body it cannot read: %v", err)
		}
		g.mu.Lock()
		n := len(g.reqs)
		g.reqs = append(g.reqs, req)
		g.mu.Unlock()
		status, body := g.reply(n, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) requests() []gwRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gwRequest(nil), g.reqs...)
}

func answer(text string) string {
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop",
	}}})
	return string(raw)
}

func callTools(calls ...[2]string) string {
	var tcs []any
	for i, c := range calls {
		tcs = append(tcs, map[string]any{
			"id": fmt.Sprintf("call_%d", i), "type": "function",
			"function": map[string]any{"name": c[0], "arguments": c[1]},
		})
	}
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"message":       map[string]any{"role": "assistant", "content": nil, "tool_calls": tcs},
		"finish_reason": "tool_calls",
	}}})
	return string(raw)
}

func gatewayError(status int, code, msg string) (int, string) {
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg, "type": "invalid_request_error", "code": code,
	}})
	return status, string(raw)
}

// startRunner runs the runtime until the test ends.
func startRunner(t *testing.T, cp *fakeControlPlane) string {
	t.Helper()
	ws := t.TempDir()
	r, err := newRunner(config{RuntimeURL: cp.srv.URL, Token: runtimeToken, AgentID: "agt_1", Workspace: ws},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r.wait = 50 * time.Millisecond
	r.backoffMin, r.backoffMax = 10*time.Millisecond, 50*time.Millisecond
	r.flushEvery = 20 * time.Millisecond
	r.gw.retryWait = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ws
}

func baseBoot(gw *fakeGateway) protocol.Bootstrap {
	return protocol.Bootstrap{
		Version: "v1",
		Agent: protocol.Agent{
			ID: "agt_1", Name: "Robin", Instructions: "Be helpful to the support team.",
			Model: "chat-default", Builtins: []string{"memory", "files"}, MaxSteps: 5,
			Owner: protocol.Person{Email: "ada@acme.ch"},
		},
		Gateway: protocol.GatewayInfo{BaseURL: gw.srv.URL + "/api/v1", APIKey: "keera_sk_first"},
	}
}

// fakeMCP is a server that answers tool calls with an event stream.
type fakeMCP struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []http.Header
	call map[string]any
}

func newFakeMCP(t *testing.T) *fakeMCP {
	m := &fakeMCP{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Errorf("MCP request %s with Accept %q", r.Method, r.Header.Get("Accept"))
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		m.mu.Lock()
		m.seen = append(m.seen, r.Header.Clone())
		m.mu.Unlock()
		if msg.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "sess-1" {
			http.Error(w, "no session", http.StatusBadRequest)
			return
		}
		reply := func(result string) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, msg.ID, result)
		}
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			reply(`{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"docs"}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			reply(`{"tools":[{"name":"search.pages","description":"Search the docs.",` +
				`"inputSchema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)
		case "tools/call":
			m.mu.Lock()
			m.call = msg.Params
			m.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\n", msg.ID)
			fmt.Fprintf(w, "data: \"result\":{\"content\":[{\"type\":\"text\",\"text\":\"Page: refunds take 5 days\"}]}}\n\n")
		default:
			t.Errorf("unexpected MCP method %q", msg.Method)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func TestRunsATaskWithBuiltinPlatformAndMCPTools(t *testing.T) {
	cp := newFakeControlPlane(t)
	mcp := newFakeMCP(t)
	gw := newFakeGateway(t, func(n int, req gwRequest) (int, string) {
		if n == 0 {
			return 200, callTools(
				[2]string{"memory_append", `{"text":"Ada prefers short answers"}`},
				[2]string{"docs__search_pages", `{"q":"refunds"}`},
				[2]string{"post_message", `{"text":"hi"}`},
				[2]string{"file_read", `{"path":"../../etc/passwd"}`},
			)
		}
		return 200, answer("Refunds take 5 days.")
	})
	boot := baseBoot(gw)
	boot.Gateway.MCP = []protocol.MCPServer{{Alias: "docs", URL: mcp.srv.URL}}
	boot.Tools = []protocol.ToolSpec{{
		Name: "post_message", Description: "Post to a channel.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
	}}
	cp.setBoot(func(b *protocol.Bootstrap) { *b = boot })
	cp.push(protocol.Task{
		ID: "tsk_1", Source: protocol.SourceChat, Prompt: "How long do refunds take?",
		RequestedBy: protocol.Person{Email: "ada@acme.ch"}, Context: "#support in Slack",
		Version: "v1",
	})
	ws := startRunner(t, cp)

	fin := cp.wait(t)
	if fin.State != protocol.Succeeded || fin.Result != "Refunds take 5 days." {
		t.Fatalf("finish = %+v", fin)
	}

	reqs := gw.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model calls, want 2", len(reqs))
	}
	first := reqs[0]
	if first.Auth != "Bearer keera_sk_first" || first.Session != "tsk_1" || first.Body.Model != "chat-default" {
		t.Errorf("first request: auth %q session %q model %q", first.Auth, first.Session, first.Body.Model)
	}
	system, user := first.Body.Messages[0].Content, first.Body.Messages[1].Content
	for _, want := range []string{"You are Robin", "ada@acme.ch is responsible", "Be helpful to the support team.", "MEMORY.md", ws} {
		if !strings.Contains(system, want) {
			t.Errorf("the system prompt lacks %q:\n%s", want, system)
		}
	}
	for _, want := range []string{"requested by ada@acme.ch", "#support in Slack", "How long do refunds take?"} {
		if !strings.Contains(user, want) {
			t.Errorf("the task message lacks %q:\n%s", want, user)
		}
	}
	var names []string
	for _, d := range first.Body.Tools {
		names = append(names, d.Function.Name)
	}
	for _, want := range []string{"memory_read", "file_search", "post_message", "docs__search_pages"} {
		if !strings.Contains(strings.Join(names, " "), want) {
			t.Errorf("tools %v lack %s", names, want)
		}
	}
	if strings.Contains(strings.Join(names, " "), "shell") {
		t.Errorf("shell is offered although it is not among the builtins: %v", names)
	}

	// The second call carries the tool results.
	results := map[string]string{}
	for _, m := range reqs[1].Body.Messages {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	if !strings.Contains(results["call_0"], "added") ||
		results["call_1"] != "Page: refunds take 5 days" ||
		results["call_2"] != "posted to #general" ||
		!strings.Contains(results["call_3"], "outside the workspace") {
		t.Errorf("tool results = %q", results)
	}
	if !strings.Contains(reqs[1].Raw, `"content":null`) {
		t.Errorf("the assistant's tool-call message should have null content: %s", reqs[1].Raw)
	}

	if mem, _ := os.ReadFile(filepath.Join(ws, "MEMORY.md")); string(mem) != "Ada prefers short answers\n" {
		t.Errorf("MEMORY.md = %q", mem)
	}
	mcp.mu.Lock()
	if mcp.call["name"] != "search.pages" {
		t.Errorf("the MCP server was called with %v", mcp.call)
	}
	last := mcp.seen[len(mcp.seen)-1]
	mcp.mu.Unlock()
	if last.Get("Authorization") != "Bearer keera_sk_first" || last.Get("X-Keera-Session") != "tsk_1" ||
		last.Get("Mcp-Protocol-Version") != "2025-06-18" {
		t.Errorf("MCP call headers = %v", last)
	}
	cp.mu.Lock()
	if len(cp.toolCalls) != 1 || cp.toolCalls[0].Path != protocol.TaskPath+"/tsk_1/tools/post_message" ||
		cp.toolCalls[0].Args["text"] != "hi" {
		t.Errorf("platform tool calls = %+v", cp.toolCalls)
	}
	cp.mu.Unlock()

	// Wait for the last batch, which may land after the finish.
	time.Sleep(50 * time.Millisecond)
	events := cp.allEvents()
	var texts []string
	for _, e := range events {
		texts = append(texts, e.Kind+": "+e.Text)
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), "keera_sk_first") || strings.Contains(string(raw), runtimeToken) {
			t.Errorf("an event holds a credential: %s", raw)
		}
	}
	all := strings.Join(texts, "\n")
	for _, want := range []string{
		"tool_call: memory_append: Ada prefers short answers",
		"tool_call: docs__search_pages",
		"tool_result: Page: refunds take 5 days",
		"message: Refunds take 5 days.",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the events lack %q:\n%s", want, all)
		}
	}
}

func TestFetchesANewKeyWhenTheGatewayRefusesTheOldOne(t *testing.T) {
	cp := newFakeControlPlane(t)
	gw := newFakeGateway(t, func(n int, req gwRequest) (int, string) {
		if req.Auth != "Bearer keera_sk_second" {
			return gatewayError(401, "invalid_api_key", "The key was revoked.")
		}
		return 200, answer("ok")
	})
	cp.setBoot(func(b *protocol.Bootstrap) { *b = baseBoot(gw) })
	startRunner(t, cp)
	time.Sleep(20 * time.Millisecond)
	cp.setBoot(func(b *protocol.Bootstrap) { b.Gateway.APIKey = "keera_sk_second" })
	cp.push(protocol.Task{ID: "tsk_1", Prompt: "hi"})

	if fin := cp.wait(t); fin.State != protocol.Succeeded || fin.Result != "ok" {
		t.Fatalf("finish = %+v", fin)
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.bootstraps != 2 {
		t.Errorf("%d bootstraps, want 2", cp.bootstraps)
	}
}

func TestATaskWithANewVersionFetchesTheBootstrapFirst(t *testing.T) {
	cp := newFakeControlPlane(t)
	gw := newFakeGateway(t, func(n int, req gwRequest) (int, string) { return 200, answer("ok") })
	cp.setBoot(func(b *protocol.Bootstrap) { *b = baseBoot(gw) })
	startRunner(t, cp)
	time.Sleep(20 * time.Millisecond)
	cp.setBoot(func(b *protocol.Bootstrap) { b.Version, b.Agent.Model = "v2", "chat-big" })
	cp.push(protocol.Task{ID: "tsk_1", Prompt: "hi", Version: "v2"})
	cp.wait(t)
	if got := gw.requests()[0].Body.Model; got != "chat-big" {
		t.Errorf("model = %q, want the new one", got)
	}
}

// A finish the control plane could not take for a while still arrives:
// giving up would leave the task running, to be handed back and run again.
func TestAFinishIsRetriedUntilItArrives(t *testing.T) {
	cp := newFakeControlPlane(t)
	cp.finishFails = 8
	gw := newFakeGateway(t, func(int, gwRequest) (int, string) { return 200, answer("done") })
	cp.setBoot(func(b *protocol.Bootstrap) { *b = baseBoot(gw) })
	cp.push(protocol.Task{ID: "tsk_1", Prompt: "hi"})
	startRunner(t, cp)
	if fin := cp.wait(t); fin.Result != "done" {
		t.Fatalf("finish = %+v", fin)
	}
	if n := len(gw.requests()); n != 1 {
		t.Errorf("%d model calls, want 1: the task ran again", n)
	}
}

func TestTheGatewaysErrorEndsTheTask(t *testing.T) {
	cp := newFakeControlPlane(t)
	gw := newFakeGateway(t, func(int, gwRequest) (int, string) {
		return gatewayError(402, "budget_exceeded", "The project's monthly budget is used up.")
	})
	cp.setBoot(func(b *protocol.Bootstrap) { *b = baseBoot(gw) })
	cp.push(protocol.Task{ID: "tsk_1", Prompt: "hi"})
	startRunner(t, cp)
	fin := cp.wait(t)
	if fin.State != protocol.Failed || !strings.Contains(fin.Error, "The project's monthly budget is used up.") {
		t.Fatalf("finish = %+v", fin)
	}
}

func TestKeepsAConversationsHistoryAndTrimsItWhenTooLong(t *testing.T) {
	cp := newFakeControlPlane(t)
	gw := newFakeGateway(t, func(n int, req gwRequest) (int, string) {
		switch n {
		case 0:
			// A tool call first: tool chatter must stay out of the history.
			return 200, callTools([2]string{"memory_read", `{}`})
		case 1:
			return 200, answer("Your order is 42.")
		case 2:
			return gatewayError(400, "context_length_exceeded", "This model's maximum context length is 8192 tokens.")
		default:
			return 200, answer("Still 42.")
		}
	})
	cp.setBoot(func(b *protocol.Bootstrap) { *b = baseBoot(gw) })
	cp.push(protocol.Task{ID: "tsk_1", Prompt: "Which order is mine?", Conversation: "slack:C1:T1"})
	cp.push(protocol.Task{ID: "tsk_2", Prompt: "And again?", Conversation: "slack:C1:T1"})
	ws := startRunner(t, cp)
	cp.wait(t)
	if fin := cp.wait(t); fin.Result != "Still 42." {
		t.Fatalf("second finish = %+v", fin)
	}

	reqs := gw.requests()
	if len(reqs) != 4 {
		t.Fatalf("%d model calls, want 4", len(reqs))
	}
	roles := func(r gwRequest) string {
		var s []string
		for _, m := range r.Body.Messages {
			s = append(s, m.Role)
		}
		return strings.Join(s, ",")
	}
	if got := roles(reqs[2]); got != "system,user,assistant,user" {
		t.Errorf("the second task's first request has %s, want its history without tool calls", got)
	}
	if !strings.Contains(reqs[2].Body.Messages[2].Content, "Your order is 42.") {
		t.Errorf("the history lacks the earlier answer: %+v", reqs[2].Body.Messages)
	}
	if got := roles(reqs[3]); got != "system,user" {
		t.Errorf("the retry has %s, want the history dropped", got)
	}

	turns := loadHistory(historyPath(ws, "slack:C1:T1"), 100)
	if len(turns) != 4 || turns[3].Content != "Still 42." {
		t.Errorf("history = %+v", turns)
	}
}

func TestStopsAtTheStepLimit(t *testing.T) {
	cp := newFakeControlPlane(t)
	gw := newFakeGateway(t, func(int, gwRequest) (int, string) {
		return 200, callTools([2]string{"memory_read", `{}`})
	})
	boot := baseBoot(gw)
	boot.Agent.MaxSteps = 3
	cp.setBoot(func(b *protocol.Bootstrap) { *b = boot })
	cp.push(protocol.Task{ID: "tsk_1", Prompt: "loop"})
	startRunner(t, cp)
	fin := cp.wait(t)
	if fin.State != protocol.Failed || !strings.Contains(fin.Error, "3 model calls") || len(gw.requests()) != 3 {
		t.Fatalf("finish = %+v after %d calls", fin, len(gw.requests()))
	}
}

func TestAsksAgainWhenTheModelWritesAToolCallAsText(t *testing.T) {
	written := `{{"name": "memory_read", "arguments": {}}}`
	for _, tc := range []struct {
		name    string
		replies []string
		want    string
	}{
		{"then answers", []string{answer(written), answer("Great Dane, Mastiff, Chihuahua.")}, protocol.Succeeded},
		{"twice", []string{answer(written), answer(written)}, protocol.Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := newFakeControlPlane(t)
			gw := newFakeGateway(t, func(n int, _ gwRequest) (int, string) { return 200, tc.replies[n] })
			cp.setBoot(func(b *protocol.Bootstrap) { *b = baseBoot(gw) })
			cp.push(protocol.Task{ID: "tsk_1", Prompt: "List dog breeds by size."})
			startRunner(t, cp)
			fin := cp.wait(t)
			if fin.State != tc.want || strings.Contains(fin.Result, "memory_read") {
				t.Fatalf("finish = %+v", fin)
			}
			if tc.want == protocol.Failed && !strings.Contains(fin.Error, "as text") {
				t.Errorf("error = %q", fin.Error)
			}
			msgs := gw.requests()[1].Body.Messages
			if last := msgs[len(msgs)-1]; len(msgs) != 4 || last.Role != "user" || !strings.Contains(last.Content, "as text") {
				t.Errorf("the second request has %+v, want the correction after the written call", msgs)
			}
		})
	}
}

func TestStopsAtTheDeadline(t *testing.T) {
	cp := newFakeControlPlane(t)
	gw := newFakeGateway(t, func(int, gwRequest) (int, string) {
		return 200, callTools([2]string{"memory_read", `{}`})
	})
	boot := baseBoot(gw)
	boot.Agent.MaxSteps = 1000
	cp.setBoot(func(b *protocol.Bootstrap) { *b = boot })
	cp.push(protocol.Task{ID: "tsk_1", Prompt: "loop", Deadline: time.Now().Add(300 * time.Millisecond)})
	startRunner(t, cp)
	fin := cp.wait(t)
	if fin.State != protocol.Failed || !strings.Contains(fin.Error, "ran out of time") {
		t.Fatalf("finish = %+v", fin)
	}
}

func TestWaitsForTheControlPlane(t *testing.T) {
	cp := newFakeControlPlane(t)
	gw := newFakeGateway(t, func(int, gwRequest) (int, string) { return 200, answer("ok") })
	// Down at first, then up.
	var mu sync.Mutex
	up := false
	inner := cp.srv.Config.Handler
	cp.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := up
		mu.Unlock()
		if !ok {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	})
	cp.setBoot(func(b *protocol.Bootstrap) { *b = baseBoot(gw) })
	cp.push(protocol.Task{ID: "tsk_1", Prompt: "hi"})
	startRunner(t, cp)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	up = true
	mu.Unlock()
	if fin := cp.wait(t); fin.State != protocol.Succeeded {
		t.Fatalf("finish = %+v", fin)
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{
		protocol.EnvRuntimeURL:   "http://keera-agents:8070/",
		protocol.EnvRuntimeToken: "tok",
		protocol.EnvAgentID:      "agt_1",
	}
	c, err := configFromEnv(func(k string) string { return env[k] })
	if err != nil || c.RuntimeURL != "http://keera-agents:8070" || c.Workspace != protocol.Workspace {
		t.Fatalf("config = %+v, %v", c, err)
	}
	delete(env, protocol.EnvRuntimeToken)
	if _, err := configFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("a missing token was accepted")
	}
}

func TestThePromptSaysWhatTheAgentIsConnectedTo(t *testing.T) {
	r := &runner{cfg: config{Workspace: "/workspace"}, ws: newWorkspace(t), now: time.Now}
	boot := protocol.Bootstrap{
		Agent: protocol.Agent{Name: "Scout", Builtins: []string{protocol.BuiltinShell}},
		Connected: []protocol.Connected{
			{ID: "bnd_1", What: protocol.ConnectedChannel, Service: "Slack", Name: "#support"},
			{
				ID: "bnd_2", What: protocol.ConnectedRepository, Service: "GitHub", Name: "acme/web",
				Clone: "git clone http://127.0.0.1:7417/bnd_2/web.git",
			},
			{ID: "bnd_3", What: protocol.ConnectedService, Service: "Jira", Name: "OPS", ReadOnly: true},
		},
	}
	got := r.systemPrompt(boot)
	for _, want := range []string{
		"## What you are connected to",
		"- Channel #support (Slack), id bnd_1\n",
		"- Repository acme/web (GitHub), id bnd_2. Clone it with: git clone http://127.0.0.1:7417/bnd_2/web.git\n",
		"- Service OPS (Jira), read only, id bnd_3\n",
		"Work in a repository with git in your shell",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}

	// Without a shell, it says it cannot clone rather than leave the model
	// to work it out.
	boot.Agent.Builtins = nil
	got = r.systemPrompt(boot)
	if strings.Contains(got, "git clone") || !strings.Contains(got, "You have no shell") {
		t.Errorf("prompt without a shell:\n%s", got)
	}

	boot.Connected = nil
	if got := r.systemPrompt(boot); strings.Contains(got, "connected to") {
		t.Errorf("prompt with nothing connected:\n%s", got)
	}
}
