package gdocs

import (
	"strings"
)

// document is what documents.get returns, as far as text goes.
type document struct {
	DocumentID string `json:"documentId"`
	Title      string `json:"title"`
	Tabs       []tab  `json:"tabs"`
	// Body and Lists are the first tab's, for a document read without
	// its tabs.
	Body  body            `json:"body"`
	Lists map[string]list `json:"lists"`
}

type tab struct {
	TabProperties struct {
		Title string `json:"title"`
	} `json:"tabProperties"`
	DocumentTab struct {
		Body  body            `json:"body"`
		Lists map[string]list `json:"lists"`
	} `json:"documentTab"`
	ChildTabs []tab `json:"childTabs"`
}

type body struct {
	Content []element `json:"content"`
}

type element struct {
	Paragraph *paragraph `json:"paragraph"`
	Table     *struct {
		TableRows []struct {
			TableCells []struct {
				Content []element `json:"content"`
			} `json:"tableCells"`
		} `json:"tableRows"`
	} `json:"table"`
}

type paragraph struct {
	Elements []struct {
		TextRun *struct {
			Content string `json:"content"`
		} `json:"textRun"`
		// Smart chips: a link to a file or a person.
		RichLink *struct {
			Properties struct {
				Title string `json:"title"`
				URI   string `json:"uri"`
			} `json:"richLinkProperties"`
		} `json:"richLink"`
		Person *struct {
			Properties struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"personProperties"`
		} `json:"person"`
	} `json:"elements"`
	ParagraphStyle struct {
		NamedStyleType string `json:"namedStyleType"`
	} `json:"paragraphStyle"`
	Bullet *struct {
		ListID       string `json:"listId"`
		NestingLevel int    `json:"nestingLevel"`
	} `json:"bullet"`
}

type list struct {
	ListProperties struct {
		NestingLevels []struct {
			GlyphType string `json:"glyphType"`
		} `json:"nestingLevels"`
	} `json:"listProperties"`
}

var headings = map[string]string{
	"TITLE": "# ", "HEADING_1": "# ", "HEADING_2": "## ", "HEADING_3": "### ",
	"HEADING_4": "#### ", "HEADING_5": "##### ", "HEADING_6": "###### ",
}

// text writes the document as plain text, each tab under its title when
// there are several.
func (d document) text() string {
	var tabs []tab
	var walk func([]tab)
	walk = func(ts []tab) {
		for _, t := range ts {
			tabs = append(tabs, t)
			walk(t.ChildTabs)
		}
	}
	walk(d.Tabs)
	var b strings.Builder
	if len(tabs) == 0 {
		writeBody(&b, d.Body.Content, d.Lists)
		return strings.TrimSpace(b.String())
	}
	for i, t := range tabs {
		if len(tabs) > 1 {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("=== Tab: " + t.TabProperties.Title + " ===\n")
		}
		writeBody(&b, t.DocumentTab.Body.Content, t.DocumentTab.Lists)
	}
	return strings.TrimSpace(b.String())
}

// writeBody skips a table of contents: it repeats the headings.
func writeBody(b *strings.Builder, content []element, lists map[string]list) {
	for _, e := range content {
		switch {
		case e.Paragraph != nil:
			writeParagraph(b, e.Paragraph, lists)
		case e.Table != nil:
			for _, row := range e.Table.TableRows {
				b.WriteString("|")
				for _, cell := range row.TableCells {
					var c strings.Builder
					writeBody(&c, cell.Content, lists)
					b.WriteString(" " + strings.Join(strings.Fields(c.String()), " ") + " |")
				}
				b.WriteString("\n")
			}
		}
	}
}

func writeParagraph(b *strings.Builder, p *paragraph, lists map[string]list) {
	var t strings.Builder
	for _, el := range p.Elements {
		switch {
		case el.TextRun != nil:
			t.WriteString(el.TextRun.Content)
		case el.RichLink != nil:
			t.WriteString(el.RichLink.Properties.Title + " (" + el.RichLink.Properties.URI + ")")
		case el.Person != nil:
			t.WriteString(el.Person.Properties.Name + " <" + el.Person.Properties.Email + ">")
		}
	}
	// A vertical tab is a line break within the paragraph.
	text := strings.ReplaceAll(strings.TrimRight(t.String(), "\n"), "\v", "\n")
	switch {
	case p.Bullet != nil:
		level := p.Bullet.NestingLevel
		mark := "- "
		if l, ok := lists[p.Bullet.ListID]; ok && level < len(l.ListProperties.NestingLevels) {
			switch l.ListProperties.NestingLevels[level].GlyphType {
			case "", "GLYPH_TYPE_UNSPECIFIED", "NONE":
			default:
				mark = "1. "
			}
		}
		b.WriteString(strings.Repeat("  ", level) + mark)
	case text != "":
		b.WriteString(headings[p.ParagraphStyle.NamedStyleType])
	}
	b.WriteString(text + "\n")
}
