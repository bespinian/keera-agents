package web

import (
	"strings"
	"testing"
)

func TestTextChunk(t *testing.T) {
	// The whole file in one go.
	c, ok := TextChunk([]byte("hello"), 0, 5)
	if !ok || c.Content != "hello" || c.Next != 0 {
		t.Fatalf("whole = %+v, %v", c, ok)
	}
	// A chunk that ends inside é leaves it for the next read.
	b := []byte(strings.Repeat("a", 9) + "é")
	c, ok = TextChunk(b[:10], 0, 20)
	if !ok || c.Content != strings.Repeat("a", 9) || c.Next != 9 {
		t.Fatalf("cut = %+v, %v", c, ok)
	}
	// One starting inside a character skips its rest.
	c, ok = TextChunk([]byte("\xa9bc"), 3, 6)
	if !ok || c.Content != "bc" {
		t.Fatalf("start = %+v, %v", c, ok)
	}
	if _, ok := TextChunk([]byte("PNG\x00\x01"), 0, 5); ok {
		t.Error("binary taken for text")
	}
	// More than a chunk is cut to one.
	c, _ = TextChunk([]byte(strings.Repeat("x", ChunkBytes+10)), 0, ChunkBytes+10)
	if len(c.Content) != ChunkBytes || c.Next != ChunkBytes {
		t.Errorf("long: %d bytes, next %d", len(c.Content), c.Next)
	}
}
