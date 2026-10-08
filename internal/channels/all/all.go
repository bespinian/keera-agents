// Package all registers every channel adapter.
package all

import (
	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/aws"
	"github.com/bespinian/keera-agents/internal/channels/azure"
	"github.com/bespinian/keera-agents/internal/channels/confluence"
	"github.com/bespinian/keera-agents/internal/channels/gcalendar"
	"github.com/bespinian/keera-agents/internal/channels/gcp"
	"github.com/bespinian/keera-agents/internal/channels/gdocs"
	"github.com/bespinian/keera-agents/internal/channels/gdrive"
	"github.com/bespinian/keera-agents/internal/channels/github"
	"github.com/bespinian/keera-agents/internal/channels/gitlab"
	"github.com/bespinian/keera-agents/internal/channels/gmail"
	"github.com/bespinian/keera-agents/internal/channels/googlechat"
	"github.com/bespinian/keera-agents/internal/channels/gsheets"
	"github.com/bespinian/keera-agents/internal/channels/gslides"
	"github.com/bespinian/keera-agents/internal/channels/hubspot"
	"github.com/bespinian/keera-agents/internal/channels/jira"
	"github.com/bespinian/keera-agents/internal/channels/kubernetes"
	"github.com/bespinian/keera-agents/internal/channels/onedrive"
	"github.com/bespinian/keera-agents/internal/channels/outlook"
	"github.com/bespinian/keera-agents/internal/channels/salesforce"
	"github.com/bespinian/keera-agents/internal/channels/slack"
	"github.com/bespinian/keera-agents/internal/channels/teams"
)

// Register creates every adapter with opts and makes it available through
// channels.AdapterFor, channels.ChatFor, channels.FilesFor, channels.CodeFor,
// channels.APIFor and channels.Descriptors. The server calls it once at start-up.
func Register(opts channels.Options) {
	for _, a := range adapters(opts) {
		channels.Register(a)
	}
}

func adapters(opts channels.Options) []channels.Adapter {
	return []channels.Adapter{
		teams.New(opts), googlechat.New(opts), slack.New(opts),
		gdrive.New(opts), onedrive.New(opts),
		github.New(opts), gitlab.New(opts),
		jira.New(opts), confluence.New(opts),
		gdocs.New(opts), gsheets.New(opts), gslides.New(opts), gcalendar.New(opts), gmail.New(opts), outlook.New(opts),
		hubspot.New(opts), salesforce.New(opts),
		kubernetes.New(opts), aws.New(opts), azure.New(opts), gcp.New(opts),
	}
}
