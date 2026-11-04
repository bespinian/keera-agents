package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ChunkBytes is how much of a file a read tool returns at once. It leaves
// room within the 64 KiB a tool may answer, so a long file is read in parts
// rather than cut.
const ChunkBytes = 48 << 10

// Chunk is part of a text file, and where the next part starts.
type Chunk struct {
	Offset int64 `json:"offset"`
	// Size is the whole file's.
	Size int64 `json:"size"`
	// Next is the offset to read from for the rest, or 0 at the end.
	Next    int64  `json:"next_offset,omitempty"`
	Content string `json:"content"`
}

// Range is the header that asks for the chunk at offset.
func Range(offset int64) string {
	return fmt.Sprintf("bytes=%d-%d", offset, offset+ChunkBytes-1)
}

// RangeTotal reads a file's whole size from a ranged answer's
// Content-Range, or from its length when the server sent it all.
func RangeTotal(resp *http.Response) int64 {
	if _, total, ok := strings.Cut(resp.Header.Get("Content-Range"), "/"); ok {
		if n, err := strconv.ParseInt(total, 10, 64); err == nil {
			return n
		}
	}
	return resp.ContentLength
}

// TextChunk turns b, read from offset of a file of total bytes, into a
// Chunk. A character cut at either end is left out, so the next read starts
// on a whole one. ok is false for content that is not text.
func TextChunk(b []byte, offset, total int64) (Chunk, bool) {
	if int64(len(b)) > ChunkBytes {
		b = b[:ChunkBytes]
	}
	read := int64(len(b))
	start := 0
	if offset > 0 {
		for start < len(b) && start < utf8.UTFMax && !utf8.RuneStart(b[start]) {
			start++
		}
	}
	end := len(b)
	if offset+read < total {
		for i := end - 1; i >= start && i >= end-utf8.UTFMax; i-- {
			if utf8.RuneStart(b[i]) {
				if !utf8.FullRune(b[i:end]) {
					end = i
				}
				break
			}
		}
	}
	text := b[start:end]
	if !utf8.Valid(text) || strings.IndexByte(string(text), 0) >= 0 {
		return Chunk{}, false
	}
	c := Chunk{Offset: offset, Size: total, Content: string(text)}
	if next := offset + int64(end); next < total {
		c.Next = next
	}
	return c, true
}
