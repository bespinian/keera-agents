package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"strings"
	"unicode/utf8"
)

// tool is one function the model may call: a builtin, a platform tool or an
// MCP server's tool.
type tool struct {
	name        string
	description string
	parameters  json.RawMessage
	// summary is the panel's line for a call, such as "shell: ls -la".
	summary func(args json.RawMessage) string
	run     func(ctx context.Context, args json.RawMessage) (string, error)
}

// toolbox is the tools of one task, in the order the model sees them.
type toolbox struct {
	log   *slog.Logger
	order []string
	tools map[string]tool
}

func newToolbox(log *slog.Logger) *toolbox { return &toolbox{log: log, tools: map[string]tool{}} }

// add keeps the first tool of a name, so a builtin cannot be shadowed by a
// platform or MCP tool.
func (b *toolbox) add(ts ...tool) {
	for _, t := range ts {
		if _, dup := b.tools[t.name]; dup {
			b.log.Warn("agent: two tools share a name; keeping the first", "tool", t.name)
			continue
		}
		if len(t.parameters) == 0 || string(t.parameters) == "null" {
			t.parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		b.order = append(b.order, t.name)
		b.tools[t.name] = t
	}
}

// writtenCallName matches the name in a tool call a model wrote out as
// text, such as {"name": "board_comment", "arguments": {...}}.
var writtenCallName = regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)

// writtenCall returns the tool a model's answer calls in text instead of
// through tool calls, or "". Some models do this when their backend does not
// parse tool calls; the call then never runs.
func (b *toolbox) writtenCall(text string) string {
	if !strings.Contains(text, `"arguments"`) && !strings.Contains(text, `"parameters"`) {
		return ""
	}
	for _, m := range writtenCallName.FindAllStringSubmatch(text, -1) {
		if _, ok := b.tools[m[1]]; ok {
			return m[1]
		}
	}
	return ""
}

func (b *toolbox) defs() []toolDef {
	defs := make([]toolDef, 0, len(b.order))
	for _, name := range b.order {
		t := b.tools[name]
		defs = append(defs, toolDef{Type: "function", Function: functionDef{
			Name: t.name, Description: t.description, Parameters: t.parameters,
		}})
	}
	return defs
}

// call runs a tool and always returns text for the model. A tool that fails
// or panics says so in the text; the task goes on.
func (b *toolbox) call(ctx context.Context, name, args string) (out string, failed bool) {
	t, ok := b.tools[name]
	if !ok {
		return fmt.Sprintf("error: there is no tool %q", name), true
	}
	raw := json.RawMessage(args)
	if strings.TrimSpace(args) == "" {
		raw = json.RawMessage(`{}`)
	}
	if !json.Valid(raw) {
		return "error: the arguments are not valid JSON: " + truncate(args, 200), true
	}
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("agent: a tool panicked", "tool", name, "panic", r, "stack", string(debug.Stack()))
			out, failed = fmt.Sprintf("error: the tool %s crashed: %v", name, r), true
		}
	}()
	res, err := t.run(ctx, raw)
	if err != nil {
		return "error: " + err.Error(), true
	}
	return res, false
}

func (b *toolbox) summary(name, args string) string {
	if t, ok := b.tools[name]; ok && t.summary != nil {
		if s := t.summary(json.RawMessage(args)); s != "" {
			return oneLine(s, 200)
		}
	}
	if args == "" || args == "{}" {
		return name
	}
	return oneLine(name+": "+args, 200)
}

// decode reads a tool's arguments into v, with a message the model can act
// on.
func decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("the arguments do not fit the tool's schema: %v", err)
	}
	return nil
}

// summarise returns a summary func that shows one string argument.
func summarise(name, field string) func(json.RawMessage) string {
	return func(args json.RawMessage) string {
		var m map[string]any
		_ = json.Unmarshal(args, &m)
		if s, ok := m[field].(string); ok && s != "" {
			return name + ": " + s
		}
		return name
	}
}

// truncate cuts s to at most n bytes on a rune boundary, and says so.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const more = "…"
	cut := max(n-len(more), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + more
}

// tail keeps the last n bytes of s on a rune boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncate(s, n)
}
