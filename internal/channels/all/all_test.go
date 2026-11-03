package all

import (
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

func TestRegister(t *testing.T) {
	Register(channels.Options{})
	want := []struct {
		kind channels.Kind
		role string
	}{
		{channels.Teams, "chat"},
		{channels.GoogleChat, "chat"},
		{channels.Slack, "chat"},
		{channels.GoogleDrive, "files"},
		{channels.OneDrive, "files"},
		{channels.GitHub, "code"},
		{channels.GitLab, "code"},
	}
	ds := channels.Descriptors()
	// A kind missing from channels' order would be left out of the list.
	if len(ds) != len(want) || len(ds) != len(adapters(channels.Options{})) {
		t.Fatalf("descriptors = %+v", ds)
	}
	for i, d := range ds {
		if d.Kind != want[i].kind {
			t.Errorf("descriptor %d is %s, want %s", i, d.Kind, want[i].kind)
		}
		if d.Label == "" || d.Setup == "" || d.TargetLabel == "" || len(d.Fields) == 0 {
			t.Errorf("%s: incomplete descriptor %+v", d.Kind, d)
		}
		// Each kind is one of chat, files and code.
		if got := role(d); got != want[i].role {
			t.Errorf("%s is %q, want %q", d.Kind, got, want[i].role)
		}
		if d.Chat != strings.Contains(d.Setup, "{webhook}") {
			t.Errorf("%s: Setup and {webhook} disagree", d.Kind)
		}
		names := map[string]bool{}
		for _, f := range d.Fields {
			if f.Name == "" || f.Label == "" || names[f.Name] {
				t.Errorf("%s: bad field %+v", d.Kind, f)
			}
			names[f.Name] = true
		}
		if _, ok := channels.AdapterFor(d.Kind); !ok {
			t.Errorf("no adapter for %s", d.Kind)
		}
	}
}

// role names what Register found a kind's adapter does.
func role(d channels.Descriptor) string {
	var roles []string
	if d.Chat {
		roles = append(roles, "chat")
	}
	if d.Files {
		roles = append(roles, "files")
	}
	if d.Code {
		roles = append(roles, "code")
	}
	return strings.Join(roles, "+")
}
