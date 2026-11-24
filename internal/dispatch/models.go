package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
)

// The model tools let an agent use other models of the gateway: one that
// turns audio into text, and chat models it may ask. They run here, not in
// the agent's container, because the audio comes from connected folders,
// which only the control plane reads. Each call goes out with the agent's
// own key, so its budget and the gateway's rules apply as to its own model.
// See docs/agents.md.

// modelTools are the tools for a's other models. folders lists its
// connected folders, from which it transcribes.
func modelTools(a store.Agent, folders string) []protocol.ToolSpec {
	var tools []protocol.ToolSpec
	if a.TranscriptionModel != "" && folders != "" {
		tools = append(tools, protocol.ToolSpec{
			Name: "model_transcribe",
			Description: fmt.Sprintf("Turn a recording in a folder you have access to into text: "+
				"audio or video, such as mp3, m4a, wav or mp4, up to %d MiB. Find it with files_list, "+
				"or take the one named in your task.", connectors.MaxReadBytes>>20) + folders,
			Parameters: schema(`{"type":"object","properties":{
				"folder":{"type":"string","description":"A folder id from the list."},
				"file_id":{"type":"string"}},
				"required":["folder","file_id"]}`),
		})
	}
	if len(a.AskModels) > 0 {
		models, _ := json.Marshal(a.AskModels)
		tools = append(tools, protocol.ToolSpec{
			Name: "model_ask",
			Description: "Ask another model and get its answer. It sees only the prompt, not your task " +
				"or tools, so put everything it needs in the prompt. Models: " +
				strings.Join(a.AskModels, ", ") + ".",
			Parameters: schema(`{"type":"object","properties":{
				"model":{"type":"string","enum":` + string(models) + `},
				"prompt":{"type":"string"}},
				"required":["model","prompt"]}`),
		})
	}
	return tools
}

func (d *Dispatcher) toolTranscribe(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	if a.TranscriptionModel == "" {
		return nil, errors.New("you have no transcription model")
	}
	f, audio, err := d.readFile(ctx, a, t, in, connectors.MaxReadBytes)
	if err != nil {
		return nil, err
	}
	if !isAudio(f) {
		return nil, fmt.Errorf("%s (%s) is not audio or video; read it with files_read", f.Name, f.MimeType)
	}
	key, err := d.fleet.OpenKey(a)
	if err != nil {
		return nil, err
	}
	text, err := d.gw.Transcribe(ctx, key, t.ID, a.TranscriptionModel, f.Name, f.MimeType, audio)
	if err != nil {
		return nil, fmt.Errorf("transcribing %s: %w", f.Name, err)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Sprintf("%s has no speech the model could hear.", f.Name), nil
	}
	return fmt.Sprintf("Transcript of %s:\n\n%s", f.Name, text), nil
}

func (d *Dispatcher) toolAsk(ctx context.Context, a store.Agent, t store.Task, in toolArgs) (any, error) {
	if !slices.Contains(a.AskModels, in.Model) {
		return nil, fmt.Errorf("you may not ask %q; ask one of %s", in.Model, strings.Join(a.AskModels, ", "))
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, errors.New("the prompt is empty")
	}
	key, err := d.fleet.OpenKey(a)
	if err != nil {
		return nil, err
	}
	answer, err := d.gw.Ask(ctx, key, t.ID, in.Model, in.Prompt)
	if err != nil {
		return nil, fmt.Errorf("asking %s: %w", in.Model, err)
	}
	return answer, nil
}

// audioExtensions name recordings whose file store gave no audio or video
// type.
var audioExtensions = []string{
	".aac", ".flac", ".m4a", ".mp3", ".mp4", ".mpeg", ".mpga", ".oga", ".ogg", ".opus", ".wav", ".webm",
}

// isAudio reports whether f is a recording a transcription model can take.
func isAudio(f connectors.File) bool {
	mime := strings.ToLower(f.MimeType)
	if strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/") {
		return true
	}
	return slices.Contains(audioExtensions, strings.ToLower(path.Ext(f.Name)))
}
