package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bespinian/keera-agents/internal/protocol"
)

// A chat message's attachments reach the model as Keera Chat sends them to a
// model: files as text before the message, images as parts after it. They
// are kept beside the chat's history, one JSON file per message, so the
// model still sees them in later messages, as in a chat with a model.

// attachmentsDir is the folder of a conversation's attachments, next to its
// history file.
func attachmentsDir(historyPath string) string {
	return strings.TrimSuffix(historyPath, ".jsonl")
}

// savedName keeps a task's id from naming a path outside the folder.
var savedName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func saveAttachments(dir, taskID string, a protocol.Attachments) error {
	if !savedName.MatchString(taskID) {
		return fmt.Errorf("task id %q cannot name a file", taskID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	// Written whole first, so a crash leaves no half a file.
	tmp := filepath.Join(dir, taskID+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, taskID+".json"))
}

// loadAttachments returns none when they are gone or damaged: the message
// still reads, without them.
func loadAttachments(dir, taskID string) protocol.Attachments {
	var a protocol.Attachments
	if !savedName.MatchString(taskID) {
		return a
	}
	raw, err := os.ReadFile(filepath.Join(dir, taskID+".json"))
	if err != nil || json.Unmarshal(raw, &a) != nil {
		return protocol.Attachments{}
	}
	return a
}

// pruneAttachments removes the attachments of turns the history no longer
// has.
func pruneAttachments(dir string, turns []turn) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for _, t := range turns {
		if t.Attachments != "" {
			keep[t.Attachments+".json"] = true
		}
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// userMessage is a message from the person with what they attached. A model
// that reads no images would refuse them, so it is told they were left out.
func userMessage(text string, a protocol.Attachments, textOnly bool) message {
	m := message{Role: "user", Content: withFiles(text, a.Files)}
	switch {
	case len(a.Images) == 0:
	case textOnly:
		n := len(a.Images)
		word := "images"
		if n == 1 {
			word = "image"
		}
		m.Content = fmt.Sprintf("[%d %s left out here: this model reads text only]\n\n%s", n, word, m.Content)
	default:
		m.Images = a.Images
	}
	return m
}

func withFiles(text string, files []protocol.File) string {
	if len(files) == 0 {
		return text
	}
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "<file name=%q>\n%s\n</file>\n\n", f.Name, strings.TrimRight(f.Text, "\n"))
	}
	b.WriteString(text)
	return strings.TrimRight(b.String(), "\n")
}
