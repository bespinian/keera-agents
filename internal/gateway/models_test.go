package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A transcription is OpenAI's form, sent with the agent's key and its task
// as the session, so the gateway bills and shows it as the agent's.
func TestTranscribeSendsTheAudioAsTheAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer sk-agent" ||
			r.Header.Get("X-Keera-Session") != "task_1" {
			t.Errorf("request: %s %s %q %q", r.Method, r.URL.Path, r.Header.Get("Authorization"),
				r.Header.Get("X-Keera-Session"))
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("model") != "whisper" || r.FormValue("response_format") != "json" {
			t.Errorf("fields = %v", r.MultipartForm.Value)
		}
		f, h, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(f)
		if h.Filename != `call "1".m4a` || h.Header.Get("Content-Type") != "audio/mp4" || string(b) != "RIFF" {
			t.Errorf("file = %q %q %q", h.Filename, h.Header.Get("Content-Type"), b)
		}
		_, _ = w.Write([]byte(`{"text":"Hello there."}`))
	}))
	defer srv.Close()
	text, err := New(srv.URL, "service").Transcribe(t.Context(), "sk-agent", "task_1", "whisper",
		`call "1".m4a`, "audio/mp4", []byte("RIFF"))
	if err != nil || text != "Hello there." {
		t.Fatalf("Transcribe = %q, %v", text, err)
	}
}

func TestAskSendsOneMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model    string `json:"model"`
			Messages []struct {
				Role, Content string
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path != "/api/v1/chat/completions" || in.Model != "keera-think" || len(in.Messages) != 1 ||
			in.Messages[0].Role != "user" || in.Messages[0].Content != "2+2?" {
			t.Errorf("request: %s %+v", r.URL.Path, in)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"4"}}]}`))
	}))
	defer srv.Close()
	answer, err := New(srv.URL, "service").Ask(t.Context(), "sk-agent", "task_1", "keera-think", "2+2?")
	if err != nil || answer != "4" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
}

// The agent reads the gateway's own reason, such as an exhausted budget.
func TestModelCallsPassOnTheGatewaysWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"code":"budget_exceeded","message":"this key's budget is spent"}}`))
	}))
	defer srv.Close()
	var heard []string
	c := New(srv.URL, "service")
	c.Observe(func(op, outcome string, _ time.Duration) { heard = append(heard, op+" "+outcome) })
	_, err := c.Ask(t.Context(), "sk-agent", "", "keera-think", "hi")
	if err == nil || err.Error() != "this key's budget is spent" {
		t.Fatalf("err = %v", err)
	}
	if len(heard) != 1 || heard[0] != "POST /api/v1/chat/completions 4xx" {
		t.Fatalf("heard %q", heard)
	}
}
