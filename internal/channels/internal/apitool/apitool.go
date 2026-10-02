// Package apitool holds what the API adapters share: a list of tools with
// the function that runs each, and reading a tool's arguments.
package apitool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bespinian/keera-agents/internal/channels"
)

// Run runs one tool. target is the binding's, which Run keeps the call
// within, or channels.AnyTarget.
type Run func(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error)

// Tool is a tool with the function that runs it.
type Tool struct {
	channels.Tool
	Run Run
}

// Set is an adapter's tools. An adapter returns Set.Tools from Tools and
// calls Set.Call from Call.
type Set []Tool

func (s Set) Tools() []channels.Tool {
	out := make([]channels.Tool, len(s))
	for i, t := range s {
		out[i] = t.Tool
	}
	return out
}

func (s Set) Call(ctx context.Context, conn channels.Connection, target, name string, args json.RawMessage) (any, error) {
	for _, t := range s {
		if t.Name == name {
			return t.Run(ctx, conn, target, args)
		}
	}
	return nil, fmt.Errorf("there is no tool called %s", name)
}

// Schema is a tool's parameters. It panics on JSON that does not parse, so
// a typo fails the adapter's tests and not a task.
func Schema(s string) json.RawMessage {
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic(fmt.Sprintf("apitool: bad schema: %v\n%s", err, s))
	}
	if v["type"] != "object" {
		panic("apitool: a tool's parameters must be an object: " + s)
	}
	return json.RawMessage(s)
}

// Decode reads a tool's arguments into a struct. No arguments are the zero
// value.
func Decode[T any](args json.RawMessage) (T, error) {
	var v T
	if len(args) == 0 || string(args) == "null" {
		return v, nil
	}
	if err := json.Unmarshal(args, &v); err != nil {
		return v, fmt.Errorf("the arguments do not fit the tool's schema: %w", err)
	}
	return v, nil
}

// Need is the error for a required argument left empty, or nil.
func Need(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("give %s", name)
	}
	return nil
}

// Limit is n, or def when n is not positive, and at most max: how many
// items a list tool asks for.
func Limit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	return min(n, max)
}
