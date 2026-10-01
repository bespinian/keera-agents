package agent

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

// A conversation's history is its user and assistant turns, one JSON line
// each, in the workspace. Tool calls stay out: they are long, and the answer
// already says what came of them.

const (
	historyTurns = 20
	// maxTurn keeps one long answer from crowding out the others.
	maxTurn = 8 << 10
	// maxHistoryFile is when a file is cut back to its recent turns.
	maxHistoryFile = 1 << 20
	keepTurns      = 200
)

type turn struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	At      time.Time `json:"at"`
}

func historyPath(ws, conversation string) string {
	sum := sha256.Sum256([]byte(conversation))
	return filepath.Join(ws, ".keera", "conversations", hex.EncodeToString(sum[:])+".jsonl")
}

// loadHistory returns the last turns of a conversation, oldest first. A
// missing or damaged file is an empty history, not a failed task.
func loadHistory(path string, n int) []turn {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err == nil && st.Size() > maxHistoryFile {
		_, _ = f.Seek(st.Size()-maxHistoryFile, io.SeekStart)
	}
	var turns []turn
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxHistoryFile)
	for sc.Scan() {
		var t turn
		if json.Unmarshal(sc.Bytes(), &t) != nil || (t.Role != "user" && t.Role != "assistant") {
			continue
		}
		turns = append(turns, t)
	}
	if len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	// A history should not open with an answer to a question it lost.
	for len(turns) > 0 && turns[0].Role != "user" {
		turns = turns[1:]
	}
	return turns
}

func appendHistory(path string, turns ...turn) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, t := range turns {
		t.Content = truncate(t.Content, maxTurn)
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(buf.Bytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > maxHistoryFile {
		return compactHistory(path)
	}
	return nil
}

// compactHistory keeps the recent turns, written to a new file first so a
// crash leaves the old one whole.
func compactHistory(path string) error {
	turns := loadHistory(path, keepTurns)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, t := range turns {
		if err := enc.Encode(t); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
