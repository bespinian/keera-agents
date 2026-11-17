package gslides

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/workspace/workspacetest"
)

const deckJSON = `{"presentationId":"P1","title":"Pitch","slides":[
	{"objectId":"s1","pageElements":[
		{"objectId":"t","shape":{"placeholder":{"type":"CENTERED_TITLE"},"text":{"textElements":[{"textRun":{"content":"Keera\n"}}]}}},
		{"objectId":"g","elementGroup":{"children":[{"objectId":"b","shape":{"text":{"textElements":[{"textRun":{"content":"Agents\u000bfor all\n"}}]}}}]}},
		{"objectId":"tb","table":{"tableRows":[{"tableCells":[{"text":{"textElements":[{"textRun":{"content":"a\n"}}]}},{"text":{"textElements":[{"textRun":{"content":"b\n"}}]}}]}]}}],
	 "slideProperties":{"notesPage":{"notesProperties":{"speakerNotesObjectId":"n"},"pageElements":[
		{"objectId":"other","shape":{"text":{"textElements":[{"textRun":{"content":"3\n"}}]}}},
		{"objectId":"n","shape":{"text":{"textElements":[{"textRun":{"content":"Smile.\n"}}]}}}]}}},
	{"objectId":"s2","pageElements":[]}]}`

func setup(t *testing.T) (*workspacetest.Fake, *Adapter, *[]map[string]any) {
	f := workspacetest.New(t)
	f.Files["P1"] = map[string]any{"id": "P1", "name": "Pitch", "mimeType": mimeType}
	f.Files["P2"] = map[string]any{"id": "P2", "name": "Other", "mimeType": mimeType}
	var updates []map[string]any
	f.Handle("GET /v1/presentations/P1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(deckJSON))
	})
	f.Handle("POST /v1/presentations/{rest}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strings.CutSuffix(r.PathValue("rest"), ":batchUpdate")
		in := workspacetest.JSON(r)
		if id != "P1" {
			workspacetest.Error(w, http.StatusBadRequest, "Invalid requests[0].createSlide: The layout is not found.")
			return
		}
		updates = append(updates, in)
		_, _ = w.Write([]byte(`{"replies":[{"replaceAllText":{"occurrencesChanged":2}}]}`))
	})
	return f, New(f.Options()), &updates
}

func call(t *testing.T, a *Adapter, conn connectors.Connection, target, tool, args string) (string, error) {
	t.Helper()
	out, err := a.Call(context.Background(), conn, target, tool, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

func requests(u map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range u["requests"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

func TestCheckAndTarget(t *testing.T) {
	f, a, _ := setup(t)
	conn := f.Conn(connectors.GoogleSlides, "")
	ctx := context.Background()
	if err := a.Check(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Scope, "auth/presentations") {
		t.Errorf("scope = %q", f.Scope)
	}
	if got, err := a.Target(ctx, conn, "https://docs.google.com/presentation/d/P1/edit#slide=id.p"); err != nil || got != "P1" {
		t.Errorf("Target = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, conn, "NOPE"); err == nil {
		t.Error("a missing presentation passed")
	}
}

func TestRead(t *testing.T) {
	f, a, _ := setup(t)
	out, err := call(t, a, f.Conn(connectors.GoogleSlides, ""), "P1", "read", `{}`)
	want := `"slides":[{"number":1,"id":"s1","title":"Keera","text":"Agents\nfor all\n| a | b |","notes":"Smile."},{"number":2,"id":"s2"}]`
	if err != nil || !strings.Contains(out, want) {
		t.Errorf("read = %s, %v", out, err)
	}
}

func TestWrites(t *testing.T) {
	f, a, updates := setup(t)
	conn := f.Conn(connectors.GoogleSlides, "")
	out, err := call(t, a, conn, "*", "replace_text", `{"presentation_id":"P1","find":"2025","replace":"2026","slide_ids":["s1"]}`)
	if err != nil || out != `{"id":"P1","occurrences_changed":2}` {
		t.Errorf("replace_text = %s, %v", out, err)
	}
	rep := requests((*updates)[0])[0]["replaceAllText"].(map[string]any)
	if rep["pageObjectIds"].([]any)[0] != "s1" {
		t.Errorf("replace sent %v", rep)
	}
	if _, err := call(t, a, conn, "P1", "replace_text", `{"find":"a","replace":"b","slide_ids":["bad id!"]}`); err == nil {
		t.Error("a bad slide id passed")
	}

	out, err = call(t, a, conn, "P1", "add_slide", `{"title":"Next","body":"One\nTwo","position":2}`)
	if err != nil || !strings.Contains(out, `"slide_id":"keera_`) {
		t.Fatalf("add_slide = %s, %v", out, err)
	}
	reqs := requests((*updates)[1])
	create := reqs[0]["createSlide"].(map[string]any)
	maps := create["placeholderIdMappings"].([]any)
	titleID := maps[0].(map[string]any)["objectId"]
	if create["insertionIndex"] != 1.0 || len(reqs) != 3 ||
		reqs[1]["insertText"].(map[string]any)["objectId"] != titleID ||
		reqs[2]["insertText"].(map[string]any)["text"] != "One\nTwo" {
		t.Errorf("add_slide sent %v", reqs)
	}
	if _, err := call(t, a, conn, "P1", "add_slide", `{}`); err != nil || len(requests((*updates)[2])) != 1 {
		t.Errorf("empty slide: %v", err)
	}
}

func TestTargetEnforced(t *testing.T) {
	f, a, updates := setup(t)
	conn := f.Conn(connectors.GoogleSlides, "")
	for tool, args := range map[string]string{
		"read":         `{"presentation_id":"P2"}`,
		"replace_text": `{"presentation_id":"P2","find":"a","replace":"b"}`,
		"add_slide":    `{"presentation_id":"P2","title":"x"}`,
	} {
		if _, err := call(t, a, conn, "P1", tool, args); err == nil || !strings.Contains(err.Error(), "only the presentation P1") {
			t.Errorf("%s outside the target: %v", tool, err)
		}
	}
	if len(*updates) > 0 {
		t.Error("a call outside the target reached Google")
	}
}

func TestAPIError(t *testing.T) {
	f, a, _ := setup(t)
	_, err := call(t, a, f.Conn(connectors.GoogleSlides, ""), "*", "add_slide", `{"presentation_id":"P2","title":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "adding the slide: Google answered 400 Bad Request") || !strings.Contains(err.Error(), "layout") {
		t.Errorf("err = %v", err)
	}
}
