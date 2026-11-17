package confluence

import (
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
)

var (
	blankLines = regexp.MustCompile(`\n{3,}`)
	anyTag     = regexp.MustCompile(`<[^>]*>`)
)

// plainText turns a page's storage format, Confluence's XHTML, into text a
// model reads easily: headings, lists, tables and code kept roughly as
// Markdown, macros' settings left out.
func plainText(storage string) string {
	in := "<body>" + storage + "</body>"
	d := xml.NewDecoder(strings.NewReader(in))
	// Storage format uses HTML's entities and the ac: and ri: prefixes
	// without declaring them, which a strict parser refuses.
	d.Strict = false
	// Not xml.HTMLAutoClose: it has link, which would close ac:link.
	d.AutoClose = []string{"br", "hr", "img", "col"}
	d.Entity = xml.HTMLEntity
	w := &textWriter{}
	var closers []func()
	for {
		tok, err := d.Token()
		if err != nil {
			// Keep the rest of a page that does not parse, without its tags.
			if rest := in[min(int(d.InputOffset()), len(in)):]; !errors.Is(err, io.EOF) && rest != "" {
				w.blank()
				w.text(anyTag.ReplaceAllString(rest, " "))
			}
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			closers = append(closers, w.open(t))
		case xml.EndElement:
			if n := len(closers); n > 0 {
				if f := closers[n-1]; f != nil {
					f()
				}
				closers = closers[:n-1]
			}
		case xml.CharData:
			w.text(string(t))
		}
	}
	lines := strings.Split(w.b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

type textWriter struct {
	b strings.Builder
	// pre is inside code, where whitespace is kept; skip inside a macro's
	// settings, which are not text; cell inside a table cell, which stays
	// on one line; image inside an image.
	pre, skip, cell, image int
	// lists holds, for each open list, 0 for bullets or the next number.
	lists []int
	// fresh is a line holding only a list marker or heading, which the
	// paragraph inside it must not break.
	fresh bool
	// linkTitle is what an ac:link shows when it has no text of its own.
	linkTitle string
}

func (w *textWriter) atSpace() bool {
	s := w.b.String()
	return s == "" || s[len(s)-1] == ' ' || s[len(s)-1] == '\n'
}

func (w *textWriter) write(s string) {
	if w.skip == 0 {
		w.b.WriteString(s)
	}
}

// mark starts a line with a list marker or a heading's hashes.
func (w *textWriter) mark(s string) {
	w.write(s)
	w.fresh = true
}

func (w *textWriter) text(s string) {
	if w.skip > 0 || s == "" {
		return
	}
	if w.pre > 0 {
		w.b.WriteString(s)
		w.fresh = false
		return
	}
	words := strings.Join(strings.Fields(s), " ")
	if words == "" || isSpace(s[0]) {
		if !w.atSpace() {
			w.b.WriteByte(' ')
		}
	}
	if words == "" {
		return
	}
	w.b.WriteString(words)
	if isSpace(s[len(s)-1]) {
		w.b.WriteByte(' ')
	}
	w.fresh = false
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\t' || c == '\r' }

// newline ends the line, if it has text.
func (w *textWriter) newline() {
	switch {
	case w.fresh || w.b.Len() == 0:
	case w.cell > 0:
		if !w.atSpace() {
			w.b.WriteByte(' ')
		}
	case !strings.HasSuffix(w.b.String(), "\n"):
		w.b.WriteByte('\n')
	}
}

// blank leaves an empty line, between paragraphs, except in a list,
// whose items' paragraphs stay together.
func (w *textWriter) blank() {
	w.newline()
	if !w.fresh && w.cell == 0 && len(w.lists) == 0 && w.b.Len() > 0 && !strings.HasSuffix(w.b.String(), "\n\n") {
		w.b.WriteByte('\n')
	}
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// open writes what starts element t and returns what ends it, or nil.
func (w *textWriter) open(t xml.StartElement) func() {
	name := t.Name.Local
	if t.Name.Space != "" {
		name = t.Name.Space + ":" + name
	}
	switch name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.blank()
		w.mark(strings.Repeat("#", int(name[1]-'0')) + " ")
		return w.blank
	case "p", "div", "blockquote", "ac:layout-section", "ac:layout-cell", "ac:rich-text-body":
		w.blank()
		return w.blank
	case "br":
		w.newline()
	case "hr":
		w.blank()
		w.write("---")
		return w.blank
	case "ul", "ol", "ac:task-list":
		n := 0
		if name == "ol" {
			n = 1
		}
		w.lists = append(w.lists, n)
		w.newline()
		return func() {
			w.lists = w.lists[:len(w.lists)-1]
			if len(w.lists) == 0 {
				w.blank()
			} else {
				w.newline()
			}
		}
	case "li", "ac:task":
		w.newline()
		marker := "- "
		if n := len(w.lists); n > 0 && w.lists[n-1] > 0 {
			marker = strconv.Itoa(w.lists[n-1]) + ". "
			w.lists[n-1]++
		}
		w.mark(strings.Repeat("  ", max(len(w.lists)-1, 0)) + marker)
		return w.newline
	case "table":
		w.blank()
		return w.blank
	case "tr":
		w.newline()
		w.write("|")
		return w.newline
	case "td", "th":
		w.write(" ")
		w.cell++
		return func() {
			w.cell--
			if !w.atSpace() {
				w.write(" ")
			}
			w.write("|")
		}
	case "pre":
		return w.fence()
	case "ac:structured-macro":
		if m := attr(t, "name"); m == "code" || m == "noformat" {
			return w.fence()
		}
		w.newline()
		return w.newline
	case "ac:parameter", "ac:task-id", "ac:task-status", "ac:placeholder", "ac:emoticon", "script", "style":
		w.skip++
		return func() { w.skip-- }
	case "code":
		if w.pre == 0 {
			w.write("`")
			return func() { w.write("`") }
		}
	case "a":
		href, start := attr(t, "href"), w.b.Len()
		return func() {
			if (strings.HasPrefix(href, "https://") || strings.HasPrefix(href, "http://")) &&
				strings.TrimSpace(w.b.String()[start:]) != href {
				w.write(" (" + href + ")")
			}
		}
	case "ac:link":
		w.linkTitle = ""
		start := w.b.Len()
		return func() {
			if w.b.Len() == start && w.linkTitle != "" {
				w.text(w.linkTitle)
			}
		}
	case "ri:page", "ri:blog-post":
		w.linkTitle = attr(t, "content-title")
	case "ri:user":
		w.linkTitle = "@user"
	case "ri:attachment", "ri:url":
		v := attr(t, "filename")
		if v == "" {
			v = attr(t, "value")
		}
		if w.image > 0 {
			w.text("[image: " + v + "]")
		} else {
			w.linkTitle = v
		}
	case "ac:image":
		w.image++
		return func() { w.image-- }
	}
	return nil
}

// fence starts a code block and returns what ends it.
func (w *textWriter) fence() func() {
	w.blank()
	w.write("```\n")
	w.pre++
	return func() {
		w.pre--
		w.newline()
		w.write("```")
		w.blank()
	}
}
