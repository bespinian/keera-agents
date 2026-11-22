package connectors

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	if got := Split("short", 100); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short = %q", got)
	}

	para := strings.Repeat("word ", 30) // 150 bytes
	text := para + "\n\n" + para + "\n\n" + para
	parts := Split(text, 200)
	if len(parts) != 3 {
		t.Fatalf("parts = %q", parts)
	}
	for _, p := range parts {
		if len(p) > 200 || strings.TrimSpace(p) != strings.TrimSpace(para) {
			t.Errorf("part = %q", p)
		}
	}

	// A code block cut in two is closed, and opened again in the next part.
	code := "Here:\n\n```\n" + strings.Repeat("line of code\n", 30) + "```\nDone."
	parts = Split(code, 200)
	if len(parts) < 2 {
		t.Fatalf("code parts = %q", parts)
	}
	for i, p := range parts {
		if len(p) > 200 || strings.Count(p, "```")%2 != 0 {
			t.Errorf("part %d = %q", i, p)
		}
		if i > 0 && !strings.HasPrefix(p, "```\n") {
			t.Errorf("part %d does not reopen the block: %q", i, p)
		}
	}
	if !strings.HasSuffix(parts[len(parts)-1], "```\nDone.") {
		t.Errorf("last = %q", parts[len(parts)-1])
	}

	// Without a break, it cuts between characters, not inside one.
	parts = Split(strings.Repeat("é", 300), 101)
	for _, p := range parts {
		if len(p) > 101 || !strings.HasPrefix(p, "é") || strings.ContainsRune(p, '\uFFFD') {
			t.Errorf("part = %q", p)
		}
	}
	if strings.Join(parts, "") != strings.Repeat("é", 300) {
		t.Error("characters were lost")
	}
}
