// Package all registers every channel adapter.
package all

import (
	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/gdrive"
	"github.com/bespinian/keera-agents/internal/channels/github"
	"github.com/bespinian/keera-agents/internal/channels/gitlab"
	"github.com/bespinian/keera-agents/internal/channels/googlechat"
	"github.com/bespinian/keera-agents/internal/channels/onedrive"
	"github.com/bespinian/keera-agents/internal/channels/slack"
	"github.com/bespinian/keera-agents/internal/channels/teams"
)

// Register creates every adapter with opts and makes it available through
// channels.AdapterFor, channels.ChatFor, channels.FilesFor, channels.CodeFor
// and channels.Descriptors. The server calls it once at start-up.
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
	}
}
