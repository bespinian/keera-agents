// Package outlook lets agents read, send and file mail in one Exchange
// Online mailbox, through Microsoft Graph. Its calendar is the
// outlookcalendar connector, so an agent may have one without the other.
//
// Endpoints:
//   - "login": Microsoft Entra, default https://login.microsoftonline.com
//   - "graph": Microsoft Graph, default https://graph.microsoft.com/v1.0
package outlook

import (
	"context"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/exchange"
	"github.com/bespinian/keera-agents/internal/connectors/internal/microsoft"
	"github.com/bespinian/keera-agents/internal/connectors/internal/oauth"
)

type Adapter struct {
	box *exchange.Client
	apitool.Set
	oauth.Accounts
}

func New(opts connectors.Options) *Adapter {
	a := &Adapter{box: exchange.NewClient(opts)}
	a.Set = a.toolSet()
	a.Accounts = a.box.Auth.Accounts([]string{
		"https://graph.microsoft.com/Mail.ReadWrite", "https://graph.microsoft.com/Mail.Send",
		"https://graph.microsoft.com/Mail.ReadWrite.Shared", "https://graph.microsoft.com/Mail.Send.Shared",
	}, "Mail.ReadWrite and Mail.Send, and their .Shared forms for mailboxes others share")
	return a
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.Outlook,
		Label:    "Outlook Mail",
		Vendor:   "Microsoft",
		Category: "Email and calendar",
		Summary:  "Agents read, send, answer and file the mail of an Exchange Online mailbox.",
		Fields:   microsoft.Fields(),
		Setup: "1. In Microsoft Entra, register an app and create a client secret.\n" +
			"2. Give the app the Microsoft Graph application permissions Mail.ReadWrite and Mail.Send. " +
			"Granted in Entra, they reach every mailbox in your tenant, so limit them to the agents' mailboxes.\n" +
			"3. Best, use Exchange Online's RBAC for Applications: create a management scope for the agents' mailboxes and " +
			"assign the app the roles Application Mail.ReadWrite and Application Mail.Send " +
			"in that scope, without granting the permissions in Entra.\n" +
			"4. Or grant them in Entra with admin consent, and restrict the app with an application access policy " +
			"(New-ApplicationAccessPolicy) to a mail-enabled security group of the agents' mailboxes.\n" +
			"5. Bind each agent to one mailbox, best one made for it.",
		TargetLabel: "Mailbox",
		TargetHelp:  exchange.TargetHelp,
		Mailbox:     true,
	}
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	return a.box.Check(ctx, conn)
}

func (a *Adapter) Target(ctx context.Context, conn connectors.Connection, target string) (string, error) {
	return a.box.Target(ctx, conn, target, "/mailFolders/inbox", "Mail.ReadWrite")
}
