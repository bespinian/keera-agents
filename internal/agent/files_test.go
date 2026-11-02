package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newWorkspace(t *testing.T) *workspace {
	t.Helper()
	w, err := openWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.root.Close() })
	return w
}

func runTool(t *testing.T, ts []tool, name, args string) (string, error) {
	t.Helper()
	for _, tl := range ts {
		if tl.name == name {
			if !json.Valid(tl.parameters) {
				t.Fatalf("%s has invalid parameters: %s", name, tl.parameters)
			}
			return tl.run(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

func TestFilesStayInTheWorkspace(t *testing.T) {
	w := newWorkspace(t)
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("s3cret"), 0o600)
	// A link the agent might have made with the shell.
	if err := os.Symlink(outside, filepath.Join(w.dir, "escape")); err != nil {
		t.Fatal(err)
	}
	ts := w.fileTools()
	for _, tc := range []struct{ tool, args string }{
		{"file_read", `{"path":"../x"}`},
		{"file_read", `{"path":"` + filepath.Join(outside, "secret") + `"}`},
		{"file_read", `{"path":"escape/secret"}`},
		{"file_write", `{"path":"escape/new","content":"x"}`},
		{"file_write", `{"path":"a/../../x","content":"x"}`},
		{"file_list", `{"path":"escape"}`},
	} {
		out, err := runTool(t, ts, tc.tool, tc.args)
		if err == nil {
			t.Errorf("%s %s escaped the workspace: %q", tc.tool, tc.args, out)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Error("a file was written outside the workspace")
	}
	// file_search does not follow the link out either.
	out, err := runTool(t, ts, "file_search", `{"pattern":"s3cret"}`)
	if err != nil || strings.Contains(out, "s3cret") {
		t.Errorf("file_search = %q, %v", out, err)
	}
}

func TestFileToolsWork(t *testing.T) {
	w := newWorkspace(t)
	ts := w.fileTools()
	if _, err := runTool(t, ts, "file_write", `{"path":"notes/a.md","content":"one\ntwo\nthree\n"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, ts, "file_write", `{"path":"notes/a.md","content":"four\n","append":true}`); err != nil {
		t.Fatal(err)
	}
	// An absolute path inside the workspace is fine.
	out, err := runTool(t, ts, "file_read", `{"path":"`+filepath.Join(w.dir, "notes/a.md")+`","offset":2,"limit":2}`)
	if err != nil || out != "two\nthree\n" {
		t.Errorf("file_read = %q, %v", out, err)
	}
	out, _ = runTool(t, ts, "file_list", `{"recursive":true}`)
	if !strings.Contains(out, "notes/\n") || !strings.Contains(out, "notes/a.md (") {
		t.Errorf("file_list = %q", out)
	}
	out, _ = runTool(t, ts, "file_search", `{"pattern":"^F","ignore_case":true,"glob":"*.md"}`)
	if out != "notes/a.md:4: four\n" {
		t.Errorf("file_search = %q", out)
	}
	if _, err := runTool(t, ts, "file_read", `{"path":"nope.txt"}`); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("reading a missing file = %v", err)
	}
	_ = os.WriteFile(filepath.Join(w.dir, "bin"), []byte{0, 1, 2}, 0o644)
	if out, _ := runTool(t, ts, "file_read", `{"path":"bin"}`); !strings.Contains(out, "binary") {
		t.Errorf("file_read of a binary = %q", out)
	}
}

func TestMemoryIsCapped(t *testing.T) {
	w := newWorkspace(t)
	ts := w.memoryTools()
	if out, _ := runTool(t, ts, "memory_read", `{}`); !strings.Contains(out, "empty") {
		t.Errorf("memory_read of nothing = %q", out)
	}
	if _, err := runTool(t, ts, "memory_append", `{"text":"one"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, ts, "memory_append", `{"text":"two"}`); err != nil {
		t.Fatal(err)
	}
	if out, _ := runTool(t, ts, "memory_read", `{}`); out != "one\ntwo\n" {
		t.Errorf("memory = %q", out)
	}
	big, _ := json.Marshal(map[string]string{"text": strings.Repeat("x", maxMemory)})
	if _, err := runTool(t, ts, "memory_append", string(big)); err == nil || !strings.Contains(err.Error(), "full") {
		t.Errorf("appending past the cap = %v", err)
	}
	big, _ = json.Marshal(map[string]string{"content": strings.Repeat("x", maxMemory+1)})
	if _, err := runTool(t, ts, "memory_write", string(big)); err == nil {
		t.Error("writing past the cap succeeded")
	}
	if _, err := runTool(t, ts, "memory_write", `{"content":"fresh"}`); err != nil || w.memory() != "fresh" {
		t.Errorf("memory_write: %v, memory %q", err, w.memory())
	}
}
