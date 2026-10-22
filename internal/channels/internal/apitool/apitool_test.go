package apitool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

func TestSet(t *testing.T) {
	set := Set{
		{Name: "echo", Run: func(_ context.Context, _ channels.Connection, target string, args json.RawMessage) (any, error) {
			in, err := Decode[struct {
				Text string `json:"text"`
			}](args)
			return target + ":" + in.Text, err
		}},
	}
	if tools := set.Tools(); len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("Tools = %+v", tools)
	}
	out, err := set.Call(context.Background(), channels.Connection{}, "ACME", "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil || out != "ACME:hi" {
		t.Fatalf("Call = %v, %v", out, err)
	}
	if _, err := set.Call(context.Background(), channels.Connection{}, "*", "nope", nil); err == nil {
		t.Error("an unknown tool ran")
	}
	if _, err := Decode[struct{ N int }](json.RawMessage(`{"N":"x"}`)); err == nil ||
		!strings.Contains(err.Error(), "schema") {
		t.Errorf("Decode of a bad argument: %v", err)
	}
}

func TestSchema(t *testing.T) {
	for _, bad := range []string{`{"type":"object"`, `{"type":"string"}`} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Schema(%s) did not panic", bad)
				}
			}()
			Schema(bad)
		}()
	}
	if Need("key", " ") == nil || Need("key", "a") != nil {
		t.Error("Need")
	}
	if Limit(0, 25, 100) != 25 || Limit(500, 25, 100) != 100 || Limit(7, 25, 100) != 7 {
		t.Error("Limit")
	}
}
