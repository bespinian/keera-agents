package runner

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bespinian/keera-agents/internal/protocol"
)

func TestVersionFollowsTheBootstrap(t *testing.T) {
	base := func() protocol.Bootstrap {
		return protocol.Bootstrap{
			Agent:   protocol.Agent{ID: "agt_1", Name: "Robin", Model: "chat-default"},
			Gateway: protocol.GatewayInfo{MCP: []protocol.MCPServer{{Alias: "docs"}}},
			Tools: []protocol.ToolSpec{{
				Name: "send_message", Description: "Your channels:\n- bnd_1: #support (slack)",
			}},
		}
	}
	v := version(base(), "key_1")

	same := base()
	same.Version, same.Gateway.APIKey = "old", "keera_sk_secret"
	if version(same, "key_1") != v {
		t.Error("the version depends on the old version or the key itself")
	}
	for name, change := range map[string]func(b *protocol.Bootstrap){
		"a binding's label": func(b *protocol.Bootstrap) { b.Tools[0].Description = "Your channels:\n- bnd_1: #help (slack)" },
		"an MCP server":     func(b *protocol.Bootstrap) { b.Gateway.MCP = nil },
		"the instructions":  func(b *protocol.Bootstrap) { b.Agent.Instructions = "Be brief." },
	} {
		b := base()
		change(&b)
		if version(b, "key_1") == v {
			t.Errorf("changing %s kept the version", name)
		}
	}
	if version(base(), "key_2") == v {
		t.Error("a new key kept the version")
	}
}

// Postgres refuses NUL bytes, and one would make every later event of the
// task fail to store.
func TestStorableDropsNULs(t *testing.T) {
	e := storable(protocol.Event{
		Kind: "tool", Text: "a\x00b\x00",
		Data: json.RawMessage(`{"output":"a\u0000b"}`),
	})
	if e.Text != "ab" || e.Data != nil {
		t.Errorf("storable = %q %s, want ab and no data", e.Text, e.Data)
	}
	ok := storable(protocol.Event{Kind: "tool", Text: "fine", Data: json.RawMessage(`{"n":1}`)})
	if ok.Text != "fine" || string(ok.Data) != `{"n":1}` {
		t.Errorf("storable changed a clean event: %q %s", ok.Text, ok.Data)
	}
	long := storable(protocol.Event{Text: strings.Repeat("\x00", 10) + strings.Repeat("é", protocol.MaxEventText)})
	if !utf8.ValidString(long.Text) || strings.ContainsRune(long.Text, 0) {
		t.Errorf("a long text was cut badly")
	}
}
