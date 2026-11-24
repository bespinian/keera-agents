package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
)

// ModelPrefix is where the gateway serves models.
const ModelPrefix = "/api/v1"

// modelTimeout bounds one model call an agent makes as a tool. It is below
// the runtime's 5 minutes for a platform tool, so the agent hears why.
const modelTimeout = 4*time.Minute + 30*time.Second

// Transcribe turns audio into text with model, as the agent whose key it is.
func (c *Client) Transcribe(ctx context.Context, key, session, model, filename, contentType string, audio []byte) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("model", model)
	_ = w.WriteField("response_format", "json")
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", multipart.FileContentDisposition("file", filename))
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	part, err := w.CreatePart(h)
	if err != nil {
		return "", err
	}
	_, _ = part.Write(audio)
	if err := w.Close(); err != nil {
		return "", err
	}
	var out struct {
		Text string `json:"text"`
	}
	err = c.model(ctx, "/audio/transcriptions", key, session, w.FormDataContentType(), &body, &out)
	return out.Text, err
}

// Ask sends one message to a chat model and returns its answer, as the agent
// whose key it is.
func (c *Client) Ask(ctx context.Context, key, session, model, prompt string) (string, error) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	in := struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
	}{Model: model, Messages: []message{{"user", prompt}}}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := c.model(ctx, "/chat/completions", key, session, "application/json", bytes.NewReader(raw), &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", errors.New("the model answered without a choice")
	}
	return out.Choices[0].Message.Content, nil
}

// model posts to the gateway's model API with an agent's key.
func (c *Client) model(ctx context.Context, path, key, session, contentType string, body io.Reader, out any) error {
	path = ModelPrefix + path
	return c.timed(ctx, modelTimeout, opName(http.MethodPost, path), func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", contentType)
		if session != "" {
			req.Header.Set(protocol.SessionHeader, session)
		}
		err = c.do(req, key, path, out)
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &Error{
				Status: http.StatusGatewayTimeout, Code: "model_timeout",
				Message: "the model took longer than " + modelTimeout.String() + " to answer",
			}
		}
		return err
	})
}
