package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/stream"
)

// workspaceRunner is a workspace runtime bootstrapped against cp, without
// its listeners or programs.
func workspaceRunner(t *testing.T, cp *fakeControlPlane, gatewayURL string) *runner {
	t.Helper()
	cp.setBoot(func(b *protocol.Bootstrap) {
		*b = protocol.Bootstrap{
			Version: "v1",
			Agent: protocol.Agent{
				ID: "wks_1", Name: "dev", Model: "chat-default", Owner: protocol.Person{Email: "ada@acme.ch"},
			},
			Gateway: protocol.GatewayInfo{BaseURL: gatewayURL + "/api/v1", APIKey: "keera_sk_ws"},
			Tools: []protocol.ToolSpec{{
				Name: "repo_list_issues", Description: "List issues.", Parameters: json.RawMessage(`{"type":"object"}`),
			}},
			Workspace: &protocol.WorkspaceInfo{
				Experience: "terminal", IngressSecret: "ingress-secret", SSHKeys: []string{"ssh-ed25519 AAAA ada"},
			},
		}
	})
	r, err := newRunner(config{RuntimeURL: cp.srv.URL, Token: runtimeToken, AgentID: "wks_1", Workspace: t.TempDir()},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBridgeAddsTheKey(t *testing.T) {
	var got http.Header
	var path string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, path = r.Header.Clone(), r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer gw.Close()
	r := workspaceRunner(t, newFakeControlPlane(t), gw.URL)
	bridge := httptest.NewServer(r.modelProxy())
	defer bridge.Close()

	req, _ := http.NewRequest(http.MethodPost, bridge.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer whatever-the-tool-has")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if path != "/api/v1/chat/completions" {
		t.Errorf("the gateway was asked for %s", path)
	}
	if got.Get("Authorization") != "Bearer keera_sk_ws" || got.Get("X-Keera-Session") != "wks_1" {
		t.Errorf("the bridge did not send the workspace's key and session: %v", got)
	}
}

func TestBridgeServesToolsOverMCP(t *testing.T) {
	cp := newFakeControlPlane(t)
	r := workspaceRunner(t, cp, "http://gateway.invalid")
	call := func(method string, params any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		w := httptest.NewRecorder()
		r.serveMCP(w, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body))))
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v: %s", method, err, w.Body)
		}
		return out
	}

	if res, _ := call("initialize", map[string]any{})["result"].(map[string]any); res["protocolVersion"] != mcpProtocol {
		t.Errorf("initialize = %v", res)
	}
	list, _ := call("tools/list", nil)["result"].(map[string]any)
	tools, _ := list["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "repo_list_issues" {
		t.Fatalf("tools/list = %v", list)
	}
	res, _ := call("tools/call", map[string]any{"name": "repo_list_issues", "arguments": map[string]any{"repository": "r1"}})["result"].(map[string]any)
	if res["isError"] != false || !strings.Contains(asJSON(res["content"]), "posted to #general") {
		t.Errorf("tools/call = %v", res)
	}
	cp.mu.Lock()
	calls := cp.toolCalls
	cp.mu.Unlock()
	if len(calls) != 1 || calls[0].Path != protocol.WorkspaceToolPath+"/repo_list_issues" || calls[0].Args["repository"] != "r1" {
		t.Errorf("the control plane saw %v", calls)
	}
	if res, _ := call("tools/call", map[string]any{"name": "nope"})["result"].(map[string]any); res["isError"] != true {
		t.Errorf("an unknown tool = %v", res)
	}
	if e, _ := call("resources/list", nil)["error"].(map[string]any); e == nil {
		t.Error("an unknown method has no error")
	}

	// A notification gets no answer.
	w := httptest.NewRecorder()
	r.serveMCP(w, httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)))
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Errorf("a notification was answered %d %q", w.Code, w.Body)
	}
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestFrontLetsOnlyKeeraAgentsIn(t *testing.T) {
	r := workspaceRunner(t, newFakeControlPlane(t), "http://gateway.invalid")
	// Something listening inside the workspace, which echoes a line.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			_, _ = c.Write([]byte("inside: " + line))
			_ = c.Close()
		}
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	front := httptest.NewServer(http.HandlerFunc(r.front))
	defer front.Close()

	open := func(secret, port string) (*bufio.Reader, func(string), error) {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/?port="+port, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		conn, err := stream.Dial(front.Client(), req)
		if err != nil {
			return nil, nil, err
		}
		t.Cleanup(func() { _ = conn.Close() })
		return bufio.NewReader(conn), func(s string) { _, _ = conn.Write([]byte(s)) }, nil
	}

	for name, secret := range map[string]string{"none": "", "wrong": "ingress-secre"} {
		_, _, err := open(secret, port)
		if se, ok := errors.AsType[*stream.Error](err); !ok || se.Status != http.StatusUnauthorized {
			t.Errorf("%s secret: %v", name, err)
		}
	}
	if _, _, err := open("ingress-secret", "1"); err == nil {
		t.Error("a port nothing listens on was joined")
	}
	read, write, err := open("ingress-secret", port)
	if err != nil {
		t.Fatal(err)
	}
	write("hello\n")
	if got, _ := read.ReadString('\n'); got != "inside: hello\n" {
		t.Errorf("read %q", got)
	}
}

func TestFrontPort(t *testing.T) {
	for _, c := range []struct {
		name, experience string
		want             int
		ok               bool
	}{
		{"ssh", "terminal", sshPort, true},
		{"web", "terminal", terminalPort, true},
		{"web", "vscode", vscodePort, true},
		{"3000", "vscode", 3000, true},
		{"0", "vscode", 0, false},
		{"http", "vscode", 0, false},
	} {
		got, ok := frontPort(c.name, c.experience)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("frontPort(%q, %q) = %d, %v", c.name, c.experience, got, ok)
		}
	}
}

func TestMergeJSONKeepsWhatTheOwnerWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pi", "models.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(path, []byte(`{"providers":{"mine":{"baseUrl":"http://x"}},"theme":"dark"}`), 0o644)
	if err := mergeJSON(path, func(m map[string]any) {
		object(m, "providers")["keera"] = piProvider([]gatewayModel{{ID: "gpt", DisplayName: "GPT", MaxContext: 1000}})
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	providers := m["providers"].(map[string]any)
	if m["theme"] != "dark" || providers["mine"] == nil || providers["keera"] == nil {
		t.Errorf("merged into %s", raw)
	}
	if !strings.Contains(string(raw), `"contextWindow": 1000`) || !strings.Contains(string(raw), bridgeURL+"/v1") {
		t.Errorf("the provider is not the bridge's: %s", raw)
	}

	_ = os.WriteFile(path, []byte("not json"), 0o644)
	if err := mergeJSON(path, func(map[string]any) {}); err == nil {
		t.Error("a file that is not JSON was replaced")
	}
	if raw, _ := os.ReadFile(path); string(raw) != "not json" {
		t.Errorf("the owner's file is now %q", raw)
	}
}

func TestFromLoopback(t *testing.T) {
	try := func(host string) (string, bool) {
		t.Helper()
		browser, front := net.Pipe()
		defer func() { _ = browser.Close() }()
		request := "GET /ws HTTP/1.1\r\nHost: " + host + "\r\n\r\n"
		answer := make(chan string, 1)
		go func() {
			_, _ = browser.Write([]byte(request))
			b, _ := io.ReadAll(browser)
			answer <- string(b)
		}()
		conn, ok := fromLoopback(front)
		if !ok {
			return <-answer, false
		}
		defer func() { _ = conn.Close() }()
		// What was read to check it still reaches the inside.
		got := make([]byte, len(request))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatal(err)
		}
		return string(got), true
	}

	for _, host := range []string{"127.0.0.1:41234", "localhost:41234", "[::1]:41234", "app.localhost"} {
		if got, ok := try(host); !ok || !strings.Contains(got, "Host: "+host) {
			t.Errorf("%s was refused or changed: %q", host, got)
		}
	}
	for _, host := range []string{"evil.example:41234", "127.0.0.1.evil.example", "10.0.0.1"} {
		if got, ok := try(host); ok || !strings.HasPrefix(got, "HTTP/1.1 421") {
			t.Errorf("%s was let in: %q", host, got)
		}
	}
}

func TestBridgeRefusesBrowsers(t *testing.T) {
	h := notFromBrowsers(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	for name, tc := range map[string]struct {
		header, value string
		want          int
	}{
		"a coding agent":   {want: http.StatusOK},
		"a web page":       {header: "Origin", value: "https://evil.example", want: http.StatusForbidden},
		"a page's no-cors": {header: "Sec-Fetch-Site", value: "cross-site", want: http.StatusForbidden},
	} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
		if tc.header != "" {
			r.Header.Set(tc.header, tc.value)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: %d, want %d", name, w.Code, tc.want)
		}
	}
	if !slices.Contains(experienceCommand("vscode", "/w").args, "--disable-proxy") {
		t.Error("code-server's proxy would let web pages reach the bridge")
	}
}
