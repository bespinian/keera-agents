package dispatch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/store"
)

const agentKey = "sk-agent-0123"

// modelCall is what the fake gateway heard of one model call.
type modelCall struct {
	path, session, model, file, audio string
}

func (g *fakeGateway) model(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+agentKey {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"not a key"}}`))
		return
	}
	c := modelCall{path: r.URL.Path, session: r.Header.Get("X-Keera-Session")}
	var answer string
	if strings.HasSuffix(r.URL.Path, "/audio/transcriptions") {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.model = r.FormValue("model")
		f, h, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(f)
		c.file, c.audio = h.Filename, string(b)
		answer = `{"text":"We ship on Friday."}`
	} else {
		var in struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		c.model = in.Model
		answer = fmt.Sprintf(`{"choices":[{"message":{"content":"%d messages"}}]}`, len(in.Messages))
	}
	g.mu.Lock()
	g.models = append(g.models, c)
	g.mu.Unlock()
	_, _ = w.Write([]byte(answer))
}

// heard is the model calls the gateway answered.
func (g *fakeGateway) heard() []modelCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]modelCall(nil), g.models...)
}

// withModels gives a a gateway key and models to use as tools.
func (e *testEnv) withModels(t *testing.T, a store.Agent, transcription string, ask ...string) store.Agent {
	t.Helper()
	sealed := e.d.box.Seal("agent-key:"+a.ID, []byte(agentKey))
	if err := e.st.ReplaceAgentKey(e.ctx, a.ID, "", "key_1", sealed, a.KeyFor()); err != nil {
		t.Fatal(err)
	}
	a, err := e.st.Agent(e.ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.TranscriptionModel, a.AskModels = transcription, ask
	if a, err = e.st.UpdateAgent(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	return a
}

// A recording in a connected folder is transcribed with the agent's own key,
// in its task's session, and only once the task may see it.
func TestModelTranscribeReadsARecordingTheTaskHasSeen(t *testing.T) {
	e := setup(t)
	files := newFakeFiles()
	connectors.Register(files)
	scout := e.withModels(t, e.agent(t, "org_a", "Scout", store.AccessOrg), "whisper")
	folder := e.bind(t, scout, e.conn(t, "org_a", filesKind, nil), "root", store.ModeFiles)
	files.put("root", connectors.File{ID: "rec", Name: "standup.m4a", MimeType: "audio/mp4"}, "AUDIO")
	files.put("root", connectors.File{ID: "doc", Name: "notes.txt", MimeType: "text/plain"}, "text")
	files.put("root", connectors.File{ID: "big", Name: "allhands.mp3", Size: connectors.MaxFileBytes + 1}, "")

	task := taskFor(scout, "usr_ada", "ada@example.ch")
	transcribe := func(file, want string) string {
		t.Helper()
		return e.call(t, scout, task, "model_transcribe", fmt.Sprintf(`{"folder":%q,"file_id":%q}`, folder.ID, file), want)
	}
	transcribe("rec", "list the folder first")
	e.call(t, scout, task, "files_list", fmt.Sprintf(`{"folder":%q}`, folder.ID), "ok")
	transcribe("doc", "is not audio or video")
	if got := transcribe("rec", "ok"); got != "Transcript of standup.m4a:\n\nWe ship on Friday." {
		t.Fatalf("transcript = %q", got)
	}
	want := modelCall{
		path: "/api/v1/audio/transcriptions", session: task.ID, model: "whisper", file: "standup.m4a", audio: "AUDIO",
	}
	if heard := e.gw.heard(); len(heard) != 1 || heard[0] != want {
		t.Fatalf("gateway heard %+v, want %+v", heard, want)
	}
	// Reading it as a file points to the tool instead of its bytes, even
	// when it is too large to read.
	for _, id := range []string{"rec", "big"} {
		got := e.call(t, scout, task, "files_read", fmt.Sprintf(`{"folder":%q,"file_id":%q}`, folder.ID, id), "ok")
		if !strings.Contains(got, "read it with model_transcribe") || strings.Contains(got, "AUDIO") {
			t.Fatalf("files_read %s = %q", id, got)
		}
	}

	// Without a transcription model, the tool refuses.
	none := e.agent(t, "org_a", "Plain", store.AccessOrg)
	e.call(t, none, taskFor(none, "usr_ada", "ada@example.ch"), "model_transcribe",
		fmt.Sprintf(`{"folder":%q,"file_id":"rec"}`, folder.ID), "no transcription model")
}

func TestModelAskReachesOnlyTheAgentsModels(t *testing.T) {
	e := setup(t)
	scout := e.withModels(t, e.agent(t, "org_a", "Scout", store.AccessOrg), "", "keera-think")
	task := taskFor(scout, "usr_ada", "ada@example.ch")
	e.call(t, scout, task, "model_ask", `{"model":"keera-other","prompt":"hi"}`, "you may not ask")
	e.call(t, scout, task, "model_ask", `{"model":"keera-think","prompt":"  "}`, "the prompt is empty")
	if got := e.call(t, scout, task, "model_ask", `{"model":"keera-think","prompt":"hi"}`, "ok"); got != "1 messages" {
		t.Fatalf("answer = %q", got)
	}
	if heard := e.gw.heard(); len(heard) != 1 || heard[0].model != "keera-think" || heard[0].session != task.ID {
		t.Fatalf("gateway heard %+v", heard)
	}
}
