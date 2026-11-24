package dispatch

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/store"
)

// fakeAPI is an API kind with a tool that reads and one that writes.
type fakeAPI struct{}

func (fakeAPI) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{Kind: "fakeapi", Label: "Fake API", TargetLabel: "Project"}
}

func (fakeAPI) Check(context.Context, connectors.Connection) error { return nil }

func (fakeAPI) Tools() []connectors.Tool {
	return []connectors.Tool{
		{Name: "search", Description: "Search.", Parameters: json.RawMessage(
			`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)},
		{Name: "create", Description: "Create.", Write: true},
	}
}

func (fakeAPI) Target(_ context.Context, _ connectors.Connection, t string) (string, error) {
	return t, nil
}

func (fakeAPI) Call(context.Context, connectors.Connection, string, string, json.RawMessage) (any, error) {
	return nil, nil
}

func TestAPITools(t *testing.T) {
	connectors.Register(fakeAPI{})
	read := store.Binding{ID: "bnd_r", Kind: "fakeapi", Target: "ACME", Mode: store.ModeRead}
	write := store.Binding{ID: "bnd_w", Kind: "fakeapi", Target: connectors.AnyTarget, Mode: store.ModeWrite}
	chat := store.Binding{ID: "bnd_c", Kind: "slack", Target: "C1", Mode: store.ModeChat}

	names := func(bs ...store.Binding) []string {
		var out []string
		for _, s := range apiTools(bs) {
			out = append(out, s.Name)
		}
		return out
	}
	if got := names(chat); got != nil {
		t.Errorf("a chat gave tools %v", got)
	}
	// A read-only binding hides the tools that write.
	if got := names(read); !slices.Equal(got, []string{"fakeapi_search"}) {
		t.Errorf("read only: %v", got)
	}
	specs := apiTools([]store.Binding{read, write})
	if len(specs) != 2 || specs[1].Name != "fakeapi_create" {
		t.Fatalf("read and write: %+v", specs)
	}

	var search, create struct {
		Properties map[string]struct {
			Enum        []string `json:"enum"`
			Description string   `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	_ = json.Unmarshal(specs[0].Parameters, &search)
	_ = json.Unmarshal(specs[1].Parameters, &create)
	if !slices.Equal(search.Required, []string{"connector", "q"}) || search.Properties["q"].Description != "" {
		t.Errorf("search's parameters: %s", specs[0].Parameters)
	}
	c := search.Properties["connector"]
	if !slices.Equal(c.Enum, []string{"bnd_r", "bnd_w"}) || !strings.Contains(c.Description, "ACME, read only") {
		t.Errorf("search's connector: %+v", c)
	}
	// Only a binding that may write is offered to a tool that writes.
	if !slices.Equal(create.Properties["connector"].Enum, []string{"bnd_w"}) {
		t.Errorf("create's connector: %+v", create.Properties["connector"])
	}

	if k, ok := apiKindOf("fakeapi_search"); !ok || k != "fakeapi" {
		t.Errorf("apiKindOf = %q, %v", k, ok)
	}
	for _, name := range []string{"send_message", "files_read", "board_comment", "repo_comment"} {
		if _, ok := apiKindOf(name); ok {
			t.Errorf("%s taken for an API tool", name)
		}
	}
}

func TestToolKind(t *testing.T) {
	connectors.Register(fakeAPI{})
	for name, want := range map[string]string{
		"fakeapi_search":    "fakeapi",
		"send_message":      "chat",
		"board_create_card": "board",
		"squad_list":        "squad",
		"files_read":        "files",
		"repo_open_change":  "repo",
		"model_transcribe":  "model",
		"nosuch_tool":       "unknown",
		"rm":                "unknown",
	} {
		if got := toolKind(name); got != want {
			t.Errorf("toolKind(%q) = %q, want %q", name, got, want)
		}
	}
}
