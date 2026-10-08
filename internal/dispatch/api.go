package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
)

// API connectors, such as Jira or AWS, give agents typed tools. They run
// here with the connector's credentials, or, for a connector people sign in
// to, as the person the task is for. They stay within the binding's target,
// and a read-only binding cannot call a tool that writes.

// maxAPIResult bounds what one API tool returns to the model, so a large
// answer does not fill its context.
const maxAPIResult = 64 << 10

func isAPI(b store.Binding) bool { return b.Mode == store.ModeRead || b.Mode == store.ModeWrite }

// apiTools lists the tools of each API kind the agent is bound to, as
// <kind>_<tool>. Each takes a connector: the id of one of the bindings. A
// tool that writes is listed only when a binding of its kind may write.
func apiTools(bindings []store.Binding) []protocol.ToolSpec {
	var kinds []string
	byKind := map[string][]store.Binding{}
	for _, b := range bindings {
		if !isAPI(b) {
			continue
		}
		if _, ok := byKind[b.Kind]; !ok {
			kinds = append(kinds, b.Kind)
		}
		byKind[b.Kind] = append(byKind[b.Kind], b)
	}
	var out []protocol.ToolSpec
	for _, kind := range kinds {
		api, ok := channels.APIFor(channels.Kind(kind))
		if !ok {
			continue
		}
		bs := byKind[kind]
		writes := slices.ContainsFunc(bs, func(b store.Binding) bool { return b.Mode == store.ModeWrite })
		for _, t := range api.Tools() {
			if t.Write && !writes {
				continue
			}
			params, err := withConnector(t.Parameters, connectorParam(kind, bs, t.Write))
			if err != nil {
				continue
			}
			out = append(out, protocol.ToolSpec{Name: kind + "_" + t.Name, Description: t.Description, Parameters: params})
		}
	}
	return out
}

// connectorParam is the schema of the connector argument: the bindings a
// tool may use, each with what it reaches.
func connectorParam(kind string, bs []store.Binding, write bool) map[string]any {
	label := kind
	if d, ok := channels.Describe(channels.Kind(kind)); ok {
		label = d.Label
	}
	var ids, lines []string
	for _, b := range bs {
		if write && b.Mode != store.ModeWrite {
			continue
		}
		ids = append(ids, b.ID)
		reach := b.Target
		if reach == channels.AnyTarget {
			reach = "everything the connector reaches"
		}
		if b.Label != "" && b.Label != b.Target {
			reach = b.Label + ", " + reach
		}
		if b.Mode == store.ModeRead {
			reach += ", read only"
		}
		lines = append(lines, fmt.Sprintf("%s (%s)", b.ID, reach))
	}
	return map[string]any{
		"type":        "string",
		"enum":        ids,
		"description": "Which " + label + " connector to use: " + strings.Join(lines, "; ") + ".",
	}
}

// withConnector adds the connector argument to a tool's parameters, first
// and required.
func withConnector(params json.RawMessage, connector map[string]any) (json.RawMessage, error) {
	var schema map[string]any
	if len(params) == 0 {
		schema = map[string]any{"type": "object"}
	} else if err := json.Unmarshal(params, &schema); err != nil {
		return nil, err
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props["connector"] = connector
	schema["properties"] = props
	required, _ := schema["required"].([]any)
	schema["required"] = append([]any{"connector"}, required...)
	return json.Marshal(schema)
}

// apiKindOf returns the API kind a tool's name starts with, if any.
func apiKindOf(name string) (channels.Kind, bool) {
	kind, _, ok := strings.Cut(name, "_")
	if !ok {
		return "", false
	}
	_, ok = channels.APIFor(channels.Kind(kind))
	return channels.Kind(kind), ok
}

// toolAPI runs an API connector's tool for the agent's task.
func (d *Dispatcher) toolAPI(ctx context.Context, a store.Agent, t store.Task, name string, raw json.RawMessage) (any, error) {
	var args map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("the arguments do not fit the tool's schema: %v", err)
		}
	}
	var connector string
	if c, ok := args["connector"]; ok {
		_ = json.Unmarshal(c, &connector)
		delete(args, "connector")
	}
	connector = strings.TrimSpace(connector)
	if connector == "" {
		return nil, errors.New("give the connector to use, from the tool's list")
	}
	b, api, conn, err := bound(ctx, d, a, t, connector, "connectors", channels.APIFor, store.ModeRead, store.ModeWrite)
	if err != nil {
		return nil, err
	}
	tool, ok := strings.CutPrefix(name, b.Kind+"_")
	if !ok {
		return nil, fmt.Errorf("%s is a %s connector; it has no tool called %s", connector, b.Kind, name)
	}
	i := slices.IndexFunc(api.Tools(), func(t channels.Tool) bool { return t.Name == tool })
	if i < 0 {
		return nil, fmt.Errorf("there is no tool called %s", name)
	}
	if api.Tools()[i].Write && b.Mode != store.ModeWrite {
		return nil, fmt.Errorf("%s is read only, so %s is not allowed", connector, name)
	}
	rest, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return api.Call(ctx, conn, b.Target, tool, rest)
}

// cutResult keeps a tool's result within maxAPIResult and says when it was
// cut, so the model asks for less instead of trusting a partial answer. The
// note fits within the bound too, or the agent's own cap would cut it off.
func cutResult(s string) string {
	if len(s) <= maxAPIResult {
		return s
	}
	note := fmt.Sprintf("\n[Cut at %d KiB of %d KiB. Ask for less, such as with a narrower query or a smaller limit.]",
		maxAPIResult>>10, len(s)>>10)
	// Not in the middle of a character.
	n := maxAPIResult - len(note)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + note
}

// LookupTarget checks an API connection reaches a binding's target, and
// returns the target as it should be stored. A connector people sign in to
// checks it as userID, who is binding it.
func (d *Dispatcher) LookupTarget(ctx context.Context, c store.Connection, target, userID string) (string, error) {
	api, ok := channels.APIFor(channels.Kind(c.Kind))
	if !ok {
		return "", fmt.Errorf("%s has no tools", c.Kind)
	}
	conn, err := d.ConnFor(ctx, c, userID)
	if err != nil {
		return "", err
	}
	return api.Target(ctx, conn, target)
}
