package connectors

import (
	"strings"
	"unicode/utf8"
)

// fence opens and closes a code block in Markdown.
const fence = "```"

// Split cuts text into parts of at most size bytes, so a long answer goes out
// as several messages: chat services refuse or cut one that is too long.
// It cuts at a paragraph break where it can, else at a line break or a
// space. A code block cut in two is closed and opened again.
func Split(text string, size int) []string {
	var parts []string
	open := false
	for {
		if open {
			text = fence + "\n" + text
		}
		if len(text) <= size {
			return append(parts, text)
		}
		// Room to close a code block the part leaves open.
		limit := max(size-len("\n"+fence), 1)
		cut := cutAt(text, limit)
		part := strings.TrimRight(text[:cut], " \n")
		open = strings.Count(part, fence)%2 == 1
		if open {
			part += "\n" + fence
		}
		parts = append(parts, part)
		text = strings.TrimLeft(text[cut:], " \n")
	}
}

// cutAt is where to cut text so the part before is at most limit bytes and
// not empty.
func cutAt(text string, limit int) int {
	head := text[:limit]
	for _, sep := range []string{"\n\n", "\n", " "} {
		// A cut in the first half would make many short parts.
		if i := strings.LastIndex(head, sep); i > limit/2 {
			return i
		}
	}
	// No break: cut between characters.
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return limit
}
