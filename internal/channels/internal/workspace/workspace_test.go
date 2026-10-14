package workspace

import (
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

func TestID(t *testing.T) {
	for in, want := range map[string]string{
		"1AbC_d-E":  "1AbC_d-E",
		" 1AbC_dE ": "1AbC_dE",
		"https://docs.google.com/document/d/1AbC_dE/edit#heading=h.x":   "1AbC_dE",
		"https://docs.google.com/spreadsheets/u/0/d/1AbC_dE/edit?gid=0": "1AbC_dE",
		"https://drive.google.com/open?id=1AbC_dE":                      "1AbC_dE",
	} {
		if got, err := parseID(in); err != nil || got != want {
			t.Errorf("parseID(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "a/b", "../x", "a b", "x' or '1", "https://docs.google.com/document/"} {
		if _, err := parseID(bad); err == nil {
			t.Errorf("parseID(%q) passed", bad)
		}
	}
}

func TestPick(t *testing.T) {
	f := Files{Noun: "document", Arg: "document_id"}
	if id, err := f.pick("DOC", ""); err != nil || id != "DOC" {
		t.Errorf("empty id on a bound target = %q, %v", id, err)
	}
	if id, err := f.pick("DOC", "https://docs.google.com/document/d/DOC/edit"); err != nil || id != "DOC" {
		t.Errorf("link to the target = %q, %v", id, err)
	}
	if _, err := f.pick("DOC", "OTHER"); err == nil {
		t.Error("another document passed")
	}
	if _, err := f.pick(channels.AnyTarget, ""); err == nil {
		t.Error("no id on AnyTarget passed")
	}
	if id, err := f.pick(channels.AnyTarget, "OTHER"); err != nil || id != "OTHER" {
		t.Errorf("AnyTarget = %q, %v", id, err)
	}
}
