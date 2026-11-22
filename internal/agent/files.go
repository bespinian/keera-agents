package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// The files and memory builtins work through an os.Root, which refuses any
// path that leaves the workspace, through ".." or through a symlink, as the
// kernel resolves it. Checking names alone would miss a symlink the agent
// made itself with the shell.

const (
	memoryFile = "MEMORY.md"
	// maxMemory keeps the memory small enough to go into every prompt.
	maxMemory = 32 << 10
	// maxReadFile is the most file_read and file_search look at in one file.
	maxReadFile = 8 << 20
	// maxReadOut is the most file_read returns at once.
	maxReadOut = 64 << 10
	// maxWriteFile bounds one file_write.
	maxWriteFile = 1 << 20
	maxListed    = 500
	maxMatches   = 200
)

type workspace struct {
	dir  string
	root *os.Root
}

func openWorkspace(dir string) (*workspace, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("making the workspace %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening the workspace %s: %w", dir, err)
	}
	return &workspace{dir: dir, root: root}, nil
}

// rel turns a path the model gave into one inside the workspace. A path
// outside it is refused here, with a clearer message than os.Root's.
func (w *workspace) rel(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return ".", nil
	}
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(w.dir, p)
		if err != nil {
			return "", fmt.Errorf("%s is outside the workspace %s", p, w.dir)
		}
		p = r
	}
	p = filepath.Clean(p)
	if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the workspace %s", p, w.dir)
	}
	return p, nil
}

// openRegular opens a regular file. O_NONBLOCK keeps a FIFO from blocking
// the open for good, as one named MEMORY.md would at every task's start;
// regular files ignore it.
func (w *workspace) openRegular(rel string, flags int) (*os.File, error) {
	f, err := w.root.OpenFile(rel, flags|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return nil, plainPathError(err)
	}
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", rel)
		if st.IsDir() {
			err = fmt.Errorf("%s is a directory; use file_list", rel)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (w *workspace) readFile(rel string, limit int64) ([]byte, int64, error) {
	f, err := w.openRegular(rel, os.O_RDONLY)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit))
	return raw, st.Size(), err
}

func (w *workspace) writeFile(rel string, data []byte, appendTo bool) error {
	if dir := filepath.Dir(rel); dir != "." {
		if err := w.root.MkdirAll(dir, 0o755); err != nil {
			return plainPathError(err)
		}
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := w.openRegular(rel, flags)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// plainPathError drops the *PathError's operation, which reads oddly to a
// model, and keeps what went wrong.
func plainPathError(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		if errors.Is(err, fs.ErrNotExist) {
			return missingError{pe.Path}
		}
		return fmt.Errorf("%s: %v", pe.Path, pe.Err)
	}
	return err
}

// missingError reads well to a model and still is fs.ErrNotExist.
type missingError struct{ path string }

func (e missingError) Error() string        { return e.path + " does not exist" }
func (e missingError) Is(target error) bool { return target == fs.ErrNotExist }

func isBinary(b []byte) bool {
	return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0
}

// memory is durable notes the agent keeps across tasks. They go into every
// task's system prompt.
func (w *workspace) memoryTools() []tool {
	return []tool{
		{
			name:        "memory_read",
			description: "Read your memory file, MEMORY.md: the durable notes you keep across tasks.",
			parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			run: func(context.Context, json.RawMessage) (string, error) {
				mem, err := w.readMemory()
				if errors.Is(err, fs.ErrNotExist) || (err == nil && mem == "") {
					return "(your memory is empty)", nil
				}
				return mem, err
			},
		},
		{
			name: "memory_write",
			description: fmt.Sprintf("Replace your whole memory file, MEMORY.md, with new content. "+
				"Use it to reorganise or shorten your notes. At most %d KiB.", maxMemory>>10),
			parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"content":{"type":"string","description":"The new content of MEMORY.md, in Markdown."}},` +
				`"required":["content"]}`),
			summary: func(args json.RawMessage) string {
				var a struct{ Content string }
				_ = json.Unmarshal(args, &a)
				return fmt.Sprintf("memory_write: %d bytes", len(a.Content))
			},
			run: func(_ context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Content string `json:"content"`
				}
				if err := decode(args, &a); err != nil {
					return "", err
				}
				if len(a.Content) > maxMemory {
					return "", fmt.Errorf("that is %d bytes and memory holds at most %d; make it shorter",
						len(a.Content), maxMemory)
				}
				if err := w.writeFile(memoryFile, []byte(a.Content), false); err != nil {
					return "", err
				}
				return fmt.Sprintf("saved; your memory now holds %d of %d bytes", len(a.Content), maxMemory), nil
			},
		},
		{
			name:        "memory_append",
			description: "Add a note to the end of your memory file, MEMORY.md, for later tasks.",
			parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"text":{"type":"string","description":"The note to add."}},"required":["text"]}`),
			summary: summarise("memory_append", "text"),
			run: func(_ context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Text string `json:"text"`
				}
				if err := decode(args, &a); err != nil {
					return "", err
				}
				old, _, err := w.readFile(memoryFile, maxMemory+1)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return "", err
				}
				add := strings.TrimRight(a.Text, "\n") + "\n"
				if len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
					add = "\n" + add
				}
				if n := len(old) + len(add); n > maxMemory {
					return "", fmt.Errorf("memory is full: %d of %d bytes used, and this adds %d; "+
						"rewrite it shorter with memory_write", len(old), maxMemory, len(add))
				}
				if err := w.writeFile(memoryFile, []byte(add), true); err != nil {
					return "", err
				}
				return fmt.Sprintf("added; your memory now holds %d of %d bytes", len(old)+len(add), maxMemory), nil
			},
		},
	}
}

// memory is the memory file for the system prompt, or empty.
func (w *workspace) memory() string {
	mem, err := w.readMemory()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(mem)
}

// readMemory reads at most maxMemory of the memory file. The file and shell
// tools can make it longer, so it then says what was left out, or the
// agent's latest notes would go missing unnoticed.
func (w *workspace) readMemory() (string, error) {
	raw, size, err := w.readFile(memoryFile, maxMemory)
	if err != nil || size <= maxMemory {
		return string(raw), err
	}
	return strings.ToValidUTF8(string(raw), "") + fmt.Sprintf("\n\n[%s is %d KiB; only its first %d KiB is shown. "+
		"Make it shorter with memory_write.]", memoryFile, size>>10, maxMemory>>10), nil
}

func (w *workspace) fileTools() []tool {
	return []tool{
		{
			name: "file_list",
			description: "List the files and directories in your workspace, or in a directory of it. " +
				"Paths are relative to the workspace.",
			parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"The directory; default the workspace itself."},` +
				`"recursive":{"type":"boolean","description":"List everything below it too."}}}`),
			summary: summarise("file_list", "path"),
			run:     w.list,
		},
		{
			name: "file_read",
			description: fmt.Sprintf("Read a text file in your workspace. Returns at most %d KiB; "+
				"use offset and limit, in lines, to read a long file in parts.", maxReadOut>>10),
			parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string"},` +
				`"offset":{"type":"integer","description":"The first line to read, from 1."},` +
				`"limit":{"type":"integer","description":"How many lines to read."}},` +
				`"required":["path"]}`),
			summary: summarise("file_read", "path"),
			run:     w.read,
		},
		{
			name: "file_write",
			description: fmt.Sprintf("Write a text file in your workspace, making its directories. "+
				"Replaces the file, or adds to its end with append. At most %d KiB.", maxWriteFile>>10),
			parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string"},"content":{"type":"string"},` +
				`"append":{"type":"boolean","description":"Add to the end instead of replacing."}},` +
				`"required":["path","content"]}`),
			summary: summarise("file_write", "path"),
			run:     w.write,
		},
		{
			name: "file_search",
			description: "Search the text files in your workspace for a regular expression, like grep. " +
				"Returns matching lines as path:line: text.",
			parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"pattern":{"type":"string","description":"A regular expression (Go syntax)."},` +
				`"path":{"type":"string","description":"The directory or file to search; default everything."},` +
				`"glob":{"type":"string","description":"Only files whose name matches, such as *.md."},` +
				`"ignore_case":{"type":"boolean"}},"required":["pattern"]}`),
			summary: summarise("file_search", "pattern"),
			run:     w.search,
		},
	}
}

func (w *workspace) list(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	rel, err := w.rel(a.Path)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	n := 0
	entry := func(name string, d fs.DirEntry) {
		if d.IsDir() {
			fmt.Fprintf(&b, "%s/\n", name)
			return
		}
		info, err := d.Info()
		if err != nil {
			fmt.Fprintf(&b, "%s\n", name)
			return
		}
		if d.Type()&fs.ModeSymlink != 0 {
			fmt.Fprintf(&b, "%s (symlink)\n", name)
			return
		}
		fmt.Fprintf(&b, "%s (%s)\n", name, size(info.Size()))
	}
	if !a.Recursive {
		entries, err := fs.ReadDir(w.root.FS(), filepath.ToSlash(rel))
		if err != nil {
			return "", plainPathError(err)
		}
		for _, d := range entries {
			if n++; n > maxListed {
				break
			}
			entry(d.Name(), d)
		}
	} else {
		err = fs.WalkDir(w.root.FS(), filepath.ToSlash(rel), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if p == filepath.ToSlash(rel) {
				return nil
			}
			if n++; n > maxListed {
				return fs.SkipAll
			}
			entry(p, d)
			if d.IsDir() && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		})
		if err != nil {
			return "", plainPathError(err)
		}
	}
	if n == 0 {
		return "(empty)", nil
	}
	if n > maxListed {
		fmt.Fprintf(&b, "[stopped after %d entries]\n", maxListed)
	}
	return b.String(), nil
}

// skipDir leaves out what is big and rarely what a search wants.
func skipDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == ".cache"
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func (w *workspace) read(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	rel, err := w.rel(a.Path)
	if err != nil {
		return "", err
	}
	raw, total, err := w.readFile(rel, maxReadFile)
	if err != nil {
		return "", err
	}
	if isBinary(raw) {
		return fmt.Sprintf("%s is a binary file of %s; it cannot be shown as text", rel, size(total)), nil
	}
	text := string(raw)
	if a.Offset > 0 || a.Limit > 0 {
		lines := strings.SplitAfter(text, "\n")
		from := max(a.Offset, 1) - 1
		if from >= len(lines) {
			return fmt.Sprintf("(%s has only %d lines)", rel, len(lines)), nil
		}
		to := len(lines)
		if a.Limit > 0 {
			to = min(from+a.Limit, len(lines))
		}
		text = strings.Join(lines[from:to], "")
	}
	if len(text) > maxReadOut {
		text = truncate(text, maxReadOut) + "\n[truncated; read on with offset and limit]"
	} else if total > int64(len(raw)) {
		text += fmt.Sprintf("\n[only the first %s of %s were read]", size(int64(len(raw))), size(total))
	}
	return text, nil
}

func (w *workspace) write(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.Path == "" {
		return "", errors.New("a path is needed")
	}
	if len(a.Content) > maxWriteFile {
		return "", fmt.Errorf("that is %s; file_write takes at most %s", size(int64(len(a.Content))), size(maxWriteFile))
	}
	rel, err := w.rel(a.Path)
	if err != nil {
		return "", err
	}
	if err := w.writeFile(rel, []byte(a.Content), a.Append); err != nil {
		return "", err
	}
	verb := "wrote"
	if a.Append {
		verb = "appended"
	}
	return fmt.Sprintf("%s %s to %s", verb, size(int64(len(a.Content))), rel), nil
}

func (w *workspace) search(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignore_case"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	pattern := a.Pattern
	if a.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("the pattern is not a valid regular expression: %v", err)
	}
	rel, err := w.rel(a.Path)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	matches := 0
	err = fs.WalkDir(w.root.FS(), filepath.ToSlash(rel), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != filepath.ToSlash(rel) && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if a.Glob != "" {
			if ok, _ := path.Match(a.Glob, d.Name()); !ok {
				return nil
			}
		}
		raw, _, err := w.readFile(filepath.FromSlash(p), maxReadFile)
		if err != nil || isBinary(raw) {
			return nil
		}
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for line := 1; sc.Scan(); line++ {
			if re.Match(sc.Bytes()) {
				fmt.Fprintf(&b, "%s:%d: %s\n", p, line, truncate(sc.Text(), 300))
				if matches++; matches >= maxMatches {
					fmt.Fprintf(&b, "[stopped after %d matches]\n", maxMatches)
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", plainPathError(err)
	}
	if matches == 0 {
		return "no matches", nil
	}
	return b.String(), nil
}
