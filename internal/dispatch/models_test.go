package dispatch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/store"
)

func TestModelTools(t *testing.T) {
	names := func(a store.Agent, folders string) []string {
		var out []string
		for _, tool := range modelTools(a, folders) {
			var v any
			if err := json.Unmarshal(tool.Parameters, &v); err != nil {
				t.Errorf("%s: %v", tool.Name, err)
			}
			out = append(out, tool.Name)
		}
		return out
	}
	if got := names(store.Agent{}, " Your folders: …"); got != nil {
		t.Errorf("without models: %v", got)
	}
	// Audio comes only from folders, so without one there is nothing to transcribe.
	if got := names(store.Agent{TranscriptionModel: "whisper"}, ""); got != nil {
		t.Errorf("without folders: %v", got)
	}
	a := store.Agent{TranscriptionModel: "whisper", AskModels: []string{"keera-think", "keera-vision"}}
	if got := names(a, " Your folders:\n- bnd_1: Calls (onedrive)"); strings.Join(got, ",") != "model_transcribe,model_ask" {
		t.Errorf("tools = %v", got)
	}
	tools := modelTools(a, " Your folders:\n- bnd_1: Calls (onedrive)")
	if !strings.Contains(tools[0].Description, "bnd_1: Calls") {
		t.Errorf("transcribe does not list the folders: %s", tools[0].Description)
	}
	if !strings.Contains(string(tools[1].Parameters), `"enum":["keera-think","keera-vision"]`) {
		t.Errorf("ask does not list the models: %s", tools[1].Parameters)
	}
}

func TestIsAudio(t *testing.T) {
	for _, c := range []struct {
		f    connectors.File
		want bool
	}{
		{connectors.File{Name: "call.m4a", MimeType: "audio/mp4"}, true},
		{connectors.File{Name: "meeting.mp4", MimeType: "video/mp4"}, true},
		{connectors.File{Name: "Memo.MP3", MimeType: "application/octet-stream"}, true},
		{connectors.File{Name: "notes.txt", MimeType: "text/plain"}, false},
		{connectors.File{Name: "slides.pdf", MimeType: "application/pdf"}, false},
	} {
		if got := isAudio(c.f); got != c.want {
			t.Errorf("isAudio(%s, %s) = %v", c.f.Name, c.f.MimeType, got)
		}
	}
}
