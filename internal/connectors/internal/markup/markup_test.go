package markup

import "testing"

func TestMrkdwn(t *testing.T) {
	for _, c := range []struct {
		in, slack, gchat string
	}{
		{"**bold** and __also__", "*bold* and *also*", "*bold* and *also*"},
		{"see [the docs](https://x.ch/a?b=1&c=2)", "see <https://x.ch/a?b=1&amp;c=2|the docs>", "see <https://x.ch/a?b=1&c=2|the docs>"},
		{"## A **big** title ##", "*A big title*", "*A big title*"},
		{"- one\n  * two", "• one\n  • two", "• one\n  • two"},
		{"~~gone~~", "~gone~", "~gone~"},
		{"a < b & c > d", "a &lt; b &amp; c &gt; d", "a < b & c > d"},
		{"keep `**code**` as is", "keep `**code**` as is", "keep `**code**` as is"},
		{"```\n# not a heading\n**x**\n```\n**y**", "```\n# not a heading\n**x**\n```\n*y*", "```\n# not a heading\n**x**\n```\n*y*"},
		{"*italic* stays", "*italic* stays", "*italic* stays"},
	} {
		if got := Mrkdwn(c.in, true); got != c.slack {
			t.Errorf("Slack %q:\n got %q\nwant %q", c.in, got, c.slack)
		}
		if got := Mrkdwn(c.in, false); got != c.gchat {
			t.Errorf("Google Chat %q:\n got %q\nwant %q", c.in, got, c.gchat)
		}
	}
}
