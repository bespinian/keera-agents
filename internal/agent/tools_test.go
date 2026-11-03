package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPanickingToolTellsTheModel(t *testing.T) {
	b := newToolbox(slog.New(slog.DiscardHandler))
	b.add(tool{name: "boom", run: func(context.Context, json.RawMessage) (string, error) {
		panic("nil map")
	}})
	out, failed := b.call(context.Background(), "boom", `{}`)
	if !failed || !strings.Contains(out, "crashed: nil map") {
		t.Errorf("call = %q, %v", out, failed)
	}
	if out, failed := b.call(context.Background(), "boom", `{bad`); !failed ||
		!strings.Contains(out, "not valid JSON") {
		t.Errorf("bad arguments = %q", out)
	}
	if out, _ := b.call(context.Background(), "nope", `{}`); !strings.Contains(out, "no tool") {
		t.Errorf("unknown tool = %q", out)
	}
}

func TestWrittenCall(t *testing.T) {
	b := newToolbox(slog.New(slog.DiscardHandler))
	b.add(tool{name: "board_comment"})
	for text, want := range map[string]string{
		`{{"name": "board_comment", "arguments": {"card_id": "c1", "body": "x"}}}`: "board_comment",
		"```json\n{\"name\":\"board_comment\",\"parameters\":{}}\n```":             "board_comment",
		`{"name": "unknown_tool", "arguments": {}}`:                                "",
		`Here is the list: {"name": "Great Dane", "weight": 70}`:                   "",
		"1. Great Dane\n2. Mastiff":                                                "",
	} {
		if got := b.writtenCall(text); got != want {
			t.Errorf("writtenCall(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestTruncateStaysWithinN(t *testing.T) {
	for s, want := range map[string]string{
		"short":   "short",
		"abcdefg": "abc…",
		"äöüx":    "ä…", // ö would end past the cut, so it goes whole
	} {
		got := truncate(s, 6)
		if got != want || len(got) > 6 {
			t.Errorf("truncate(%q, 6) = %q, want %q", s, got, want)
		}
	}
}

func TestMCPToolName(t *testing.T) {
	if got := mcpToolName("git hub", "search.code"); got != "git_hub__search_code" {
		t.Errorf("mcpToolName = %q", got)
	}
	a, b := mcpToolName("x", strings.Repeat("a", 80)+"1"), mcpToolName("x", strings.Repeat("a", 80)+"2")
	if len(a) > 64 || a == b {
		t.Errorf("long names %q and %q", a, b)
	}
}

func TestReadSSEReplySkipsOtherMessages(t *testing.T) {
	stream := ": keep-alive\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"ping\",\"id\":99}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":7,\"error\":{\"code\":-32602,\"message\":\"unknown tool\"}}\n\n"
	m, err := readSSEReply(strings.NewReader(stream), json.RawMessage("7"))
	if err != nil || m.Error == nil || m.Error.Message != "unknown tool" {
		t.Fatalf("reply = %+v, %v", m, err)
	}
	if _, err := readSSEReply(strings.NewReader("data: {}\n\n"), json.RawMessage("7")); err == nil {
		t.Error("a stream without the answer gave no error")
	}
}

func TestToolResultText(t *testing.T) {
	got, isError := toolResultText(json.RawMessage(`{"content":[{"type":"text","text":"a"},` +
		`{"type":"image","mimeType":"image/png","data":"..."}],"isError":true}`))
	if got != "a\n[image image/png omitted]" || !isError {
		t.Errorf("toolResultText = %q, %v", got, isError)
	}
}

func TestMCPReconnectsAForgottenSession(t *testing.T) {
	inits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg rpcMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		switch msg.Method {
		case "initialize":
			inits++
			w.Header().Set("Mcp-Session-Id", "s"+string(rune('0'+inits)))
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) + `,"result":{"protocolVersion":"2025-06-18"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if r.Header.Get("Mcp-Session-Id") != "s2" {
				http.Error(w, "session expired", http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(msg.ID) +
				`,"result":{"content":[{"type":"text","text":"ok"}]}}`))
		}
	}))
	defer srv.Close()
	c := &mcpClient{alias: "x", url: srv.URL, http: srv.Client(), key: func() string { return "k" }}
	if err := c.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := c.call(context.Background(), "", "t", json.RawMessage(`{}`))
	if err != nil || out != "ok" || inits != 2 {
		t.Errorf("call = %q, %v after %d initializations", out, err, inits)
	}
}

func TestHTMLText(t *testing.T) {
	got := htmlText(`<html><head><title>T</title><style>p{}</style></head><body>` +
		`<h1>Refunds</h1><script>alert(1)</script><p>Take&nbsp;5 &amp; more <b>days</b>.</p></body></html>`)
	if got != "Refunds\nTake 5 & more days ." {
		t.Errorf("htmlText = %q", got)
	}
}

func TestWebFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<p>hello</p>"))
	}))
	defer srv.Close()
	out, err := fetch(context.Background(), srv.Client(), srv.URL)
	if err != nil || !strings.HasPrefix(out, "HTTP 200 text/html") || !strings.HasSuffix(out, "hello") {
		t.Errorf("fetch = %q, %v", out, err)
	}
	if _, err := fetch(context.Background(), srv.Client(), "file:///etc/passwd"); err == nil {
		t.Error("a file URL was fetched")
	}
}
