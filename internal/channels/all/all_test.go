package all

import (
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

func TestRegister(t *testing.T) {
	Register(channels.Options{})
	want := []channels.Kind{
		channels.Teams, channels.GoogleChat, channels.Slack, channels.GoogleDrive, channels.OneDrive,
		channels.GitHub, channels.GitLab,
	}
	ds := channels.Descriptors()
	// A kind missing from channels' order would be left out of the list.
	if len(ds) != len(want) || len(ds) != len(adapters(channels.Options{})) {
		t.Fatalf("descriptors = %+v", ds)
	}
	for i, d := range ds {
		if d.Kind != want[i] {
			t.Errorf("descriptor %d is %s, want %s", i, d.Kind, want[i])
		}
		if d.Label == "" || d.Setup == "" || d.TargetLabel == "" || len(d.Fields) == 0 {
			t.Errorf("%s: incomplete descriptor %+v", d.Kind, d)
		}
		// Each kind is one of chat, files and code.
		if n := btoi(d.Chat) + btoi(d.Files) + btoi(d.Code); n != 1 {
			t.Errorf("%s: chat %v files %v code %v", d.Kind, d.Chat, d.Files, d.Code)
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
	}
	for _, k := range []channels.Kind{channels.Teams, channels.GoogleChat, channels.Slack} {
		if _, ok := channels.ChatFor(k); !ok {
			t.Errorf("no chat for %s", k)
		}
	}
	for _, k := range []channels.Kind{channels.GoogleDrive, channels.OneDrive} {
		if _, ok := channels.FilesFor(k); !ok {
			t.Errorf("no files for %s", k)
		}
	}
	for _, k := range []channels.Kind{channels.GitHub, channels.GitLab} {
		if _, ok := channels.CodeFor(k); !ok {
			t.Errorf("no code host for %s", k)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
