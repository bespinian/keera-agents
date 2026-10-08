package all

import (
	"encoding/json"
	"regexp"
	"slices"
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
		{channels.Jira, "api"},
		{channels.Confluence, "api"},
		{channels.GoogleDocs, "api"},
		{channels.GoogleSheets, "api"},
		{channels.GoogleSlides, "api"},
		{channels.GoogleCalendar, "api"},
		{channels.Gmail, "api"},
		{channels.Outlook, "api"},
		{channels.HubSpot, "api"},
		{channels.Salesforce, "api"},
		{channels.Kubernetes, "api"},
		{channels.AWS, "api"},
		{channels.Azure, "api"},
		{channels.GCP, "api"},
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
		if d.Label == "" || d.Vendor == "" || d.Category == "" || d.Summary == "" || d.Setup == "" ||
			len(d.Fields) == 0 {
			t.Errorf("%s: incomplete descriptor %+v", d.Kind, d)
		}
		// Only an API kind may name nothing, and only it may reach everything.
		if !d.API && (d.TargetLabel == "" || d.AnyTarget) {
			t.Errorf("%s: target_label %q with any_target %v", d.Kind, d.TargetLabel, d.AnyTarget)
		}
		if d.API {
			checkTools(t, d)
		}
		// Each kind is one of chat, files, code and api.
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
		checkSignIn(t, d)
	}
}

// signIns are the kinds people may sign in to with their own accounts.
var signIns = []channels.Kind{
	channels.GoogleDrive, channels.OneDrive, channels.GitHub, channels.GitLab,
	channels.Jira, channels.Confluence,
	channels.GoogleDocs, channels.GoogleSheets, channels.GoogleSlides, channels.GoogleCalendar, channels.Gmail, channels.Outlook,
	channels.Salesforce, channels.Kubernetes, channels.AWS, channels.Azure, channels.GCP,
}

// checkSignIn checks a kind people sign in to says how to set it up. A chat
// may not be one: its bot is the organisation's, and who may talk to an
// agent is decided by the sender.
func checkSignIn(t *testing.T, d channels.Descriptor) {
	t.Helper()
	_, perUser := channels.PerUserFor(d.Kind)
	if want := slices.Contains(signIns, d.Kind); (d.SignIn != nil) != want || perUser != want {
		t.Errorf("%s: sign_in %v, PerUser %v, want %v", d.Kind, d.SignIn != nil, perUser, want)
	}
	if d.SignIn == nil {
		return
	}
	if d.Chat || len(d.SignIn.Fields) == 0 || !strings.Contains(d.SignIn.Setup, "{callback}") {
		t.Errorf("%s: incomplete sign-in %+v", d.Kind, d.SignIn)
	}
}

// toolName is what models accept as a tool's name, with room for the
// kind's prefix.
var toolName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// checkTools checks an API kind's tools are ones a model can be given.
func checkTools(t *testing.T, d channels.Descriptor) {
	t.Helper()
	if len(d.Tools) == 0 {
		t.Errorf("%s has no tools", d.Kind)
	}
	names := map[string]bool{}
	reads := false
	for _, tool := range d.Tools {
		full := string(d.Kind) + "_" + tool.Name
		if !toolName.MatchString(tool.Name) || len(full) > 64 || names[tool.Name] || tool.Name == "connector" {
			t.Errorf("%s: bad tool name %q", d.Kind, tool.Name)
		}
		names[tool.Name] = true
		reads = reads || !tool.Write
		if tool.Description == "" {
			t.Errorf("%s: %s has no description", d.Kind, full)
		}
		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if len(tool.Parameters) > 0 {
			if err := json.Unmarshal(tool.Parameters, &schema); err != nil || schema.Type != "object" {
				t.Errorf("%s: %s's parameters are not an object: %v", d.Kind, full, err)
			}
		}
		// Dispatch adds it, naming the channel.
		if _, ok := schema.Properties["connector"]; ok {
			t.Errorf("%s: %s takes connector itself", d.Kind, full)
		}
	}
	// A channel that may only read must still have something to do.
	if !reads {
		t.Errorf("%s has no tool that only reads", d.Kind)
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
	if d.API {
		roles = append(roles, "api")
	}
	return strings.Join(roles, "+")
}
