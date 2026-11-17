// Package all registers every connector adapter.
package all

import (
	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/aws"
	"github.com/bespinian/keera-agents/internal/connectors/azure"
	"github.com/bespinian/keera-agents/internal/connectors/confluence"
	"github.com/bespinian/keera-agents/internal/connectors/gcalendar"
	"github.com/bespinian/keera-agents/internal/connectors/gcp"
	"github.com/bespinian/keera-agents/internal/connectors/gdocs"
	"github.com/bespinian/keera-agents/internal/connectors/gdrive"
	"github.com/bespinian/keera-agents/internal/connectors/github"
	"github.com/bespinian/keera-agents/internal/connectors/gitlab"
	"github.com/bespinian/keera-agents/internal/connectors/gmail"
	"github.com/bespinian/keera-agents/internal/connectors/googlechat"
	"github.com/bespinian/keera-agents/internal/connectors/gsheets"
	"github.com/bespinian/keera-agents/internal/connectors/gslides"
	"github.com/bespinian/keera-agents/internal/connectors/hubspot"
	"github.com/bespinian/keera-agents/internal/connectors/jira"
	"github.com/bespinian/keera-agents/internal/connectors/kubernetes"
	"github.com/bespinian/keera-agents/internal/connectors/onedrive"
	"github.com/bespinian/keera-agents/internal/connectors/outlook"
	"github.com/bespinian/keera-agents/internal/connectors/postgres"
	"github.com/bespinian/keera-agents/internal/connectors/salesforce"
	"github.com/bespinian/keera-agents/internal/connectors/slack"
	"github.com/bespinian/keera-agents/internal/connectors/teams"
)

// Register creates every adapter with opts and makes it available through
// connectors.AdapterFor, ChatFor, FilesFor, CodeFor, APIFor, PerUserFor and
// Descriptors. The server calls it once at start-up.
func Register(opts connectors.Options) {
	for _, a := range adapters(opts) {
		connectors.Register(a)
	}
}

// adapters are in the order the panel lists them: chats first, then files,
// code hosts and the rest.
func adapters(opts connectors.Options) []connectors.Adapter {
	return []connectors.Adapter{
		teams.New(opts), googlechat.New(opts), slack.New(opts),
		gdrive.New(opts), onedrive.New(opts),
		github.New(opts), gitlab.New(opts),
		jira.New(opts), confluence.New(opts),
		gdocs.New(opts), gsheets.New(opts), gslides.New(opts), gcalendar.New(opts), gmail.New(opts), outlook.New(opts),
		hubspot.New(opts), salesforce.New(opts), postgres.New(opts),
		kubernetes.New(opts), aws.New(opts), azure.New(opts), gcp.New(opts),
	}
}
