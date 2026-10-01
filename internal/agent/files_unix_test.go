//go:build unix

package agent

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO in the workspace, as one made with mkfifo, is refused rather than
// opened: opening it would block until a writer came, which never happens.
func TestFIFOsAreRefused(t *testing.T) {
	w := newWorkspace(t)
	for _, name := range []string{memoryFile, "pipe"} {
		if err := syscall.Mkfifo(filepath.Join(w.dir, name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if mem := w.memory(); mem != "" {
			t.Errorf("memory = %q", mem)
		}
		ts := w.fileTools()
		if _, err := runTool(t, ts, "file_read", `{"path":"pipe"}`); err == nil ||
			!strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("file_read on a FIFO: %v", err)
		}
		if _, err := runTool(t, w.memoryTools(), "memory_append", `{"text":"note"}`); err == nil {
			t.Error("memory_append wrote to a FIFO")
		}
		if _, err := runTool(t, ts, "file_write", `{"path":"pipe","content":"x"}`); err == nil {
			t.Error("file_write wrote to a FIFO")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO blocked the workspace")
	}
}
