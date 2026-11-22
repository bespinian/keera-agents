package protocol

import (
	"strings"
	"testing"
)

func TestPort(t *testing.T) {
	for name, want := range map[string]int{"web": 0, "ssh": 0, "3000": 3000, "65535": 65535} {
		if got, ok := Port(name); !ok || got != want {
			t.Errorf("Port(%q) = %d, %v, want %d", name, got, ok, want)
		}
	}
	for _, name := range []string{"", "0", "65536", "03000", "+3000", "http", "-1"} {
		if _, ok := Port(name); ok {
			t.Errorf("Port(%q) is valid", name)
		}
	}
}

func TestCutResult(t *testing.T) {
	if got := CutResult("short"); got != "short" {
		t.Errorf("cut a short result: %q", got)
	}
	long := strings.Repeat("é", MaxToolResult)
	got := CutResult(long)
	head, note, ok := strings.Cut(got, "\n[Cut")
	if !ok || !strings.Contains(note, "Ask for less") || len(got) > MaxToolResult {
		t.Fatalf("cut to %d bytes: %q", len(got), note)
	}
	if !strings.HasSuffix(head, "é") {
		t.Error("cut in the middle of a character")
	}
}
