package id

import (
	"strings"
	"testing"
	"time"
)

func TestNewIsUniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]bool, 10000)
	for range 10000 {
		got := New("org")
		if !strings.HasPrefix(got, "org_") {
			t.Fatalf("New(\"org\") = %q", got)
		}
		if len(got) != len("org_")+26 {
			t.Fatalf("New(\"org\") = %q, want 26 characters after the prefix", got)
		}
		if seen[got] {
			t.Fatalf("New returned %q twice", got)
		}
		seen[got] = true
	}
}

func TestNewSortsByCreationTime(t *testing.T) {
	// Sortable ids are what make "ORDER BY id" a chronological listing.
	first := New("key")
	time.Sleep(2 * time.Millisecond)
	second := New("key")
	if first >= second {
		t.Errorf("%q was minted before %q but does not sort before it", first, second)
	}
}
