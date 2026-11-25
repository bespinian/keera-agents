package store

import (
	"slices"
	"strings"
	"testing"
)

func TestCleanLabels(t *testing.T) {
	long := strings.Repeat("a", MaxLabel-1) + "é"
	got := CleanLabels([]string{" bug ", "", "bug", "  ", long})
	want := []string{"bug", strings.Repeat("a", MaxLabel-1)}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if CleanLabels([]string{" "}) != nil {
		t.Fatal("only empty labels should give none")
	}
}
