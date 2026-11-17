// Package markup turns the Markdown agents write into the simpler markup
// Slack and Google Chat show: *bold*, ~strike~ and <url|text> links.
package markup

import (
	"regexp"
	"strings"
)

var (
	heading = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	bullet  = regexp.MustCompile(`^(\s*)[-*+]\s+`)
	link    = regexp.MustCompile(`!?\[([^\]]+)\]\(([^)\s]+)\)`)
	bold    = regexp.MustCompile(`\*\*(.+?)\*\*|__(.+?)__`)
	strike  = regexp.MustCompile(`~~(.+?)~~`)
	escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
)

// Mrkdwn converts Markdown line by line and leaves code alone. escape is
// for Slack, where &, < and > are control characters.
func Mrkdwn(md string, escape bool) string {
	lines := strings.Split(md, "\n")
	fenced := false
	for i, line := range lines {
		if escape {
			line = escaper.Replace(line)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			lines[i] = line
			continue
		}
		if fenced {
			lines[i] = line
			continue
		}
		if m := heading.FindStringSubmatch(line); m != nil {
			text := strings.ReplaceAll(strings.ReplaceAll(m[1], "**", ""), "__", "")
			lines[i] = "*" + inline(text) + "*"
			continue
		}
		line = bullet.ReplaceAllString(line, "${1}• ")
		lines[i] = inline(line)
	}
	return strings.Join(lines, "\n")
}

// inline converts the parts of a line outside `code` spans.
func inline(line string) string {
	parts := strings.Split(line, "`")
	for i := 0; i < len(parts); i += 2 {
		p := link.ReplaceAllString(parts[i], "<$2|$1>")
		p = bold.ReplaceAllString(p, "*$1$2*")
		parts[i] = strike.ReplaceAllString(p, "~$1~")
	}
	return strings.Join(parts, "`")
}
