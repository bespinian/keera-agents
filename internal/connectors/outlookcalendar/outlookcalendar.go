// Package outlookcalendar lets agents read and keep the calendar of one
// Exchange Online mailbox, through Microsoft Graph. Its mail is the outlook
// connector, so an agent may have one without the other.
//
// Endpoints:
//   - "login": Microsoft Entra, default https://login.microsoftonline.com
//   - "graph": Microsoft Graph, default https://graph.microsoft.com/v1.0
package outlookcalendar

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
		"https://graph.microsoft.com/Calendars.ReadWrite", "https://graph.microsoft.com/Calendars.ReadWrite.Shared",
	}, "Calendars.ReadWrite, and Calendars.ReadWrite.Shared for calendars others share")
	return a
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.OutlookCalendar,
		Label:    "Outlook Calendar",
		Vendor:   "Microsoft",
		Category: "Email and calendar",
		Summary:  "Agents read and keep the calendar of an Exchange Online mailbox, and invite people to Teams meetings.",
		Fields:   microsoft.Fields(),
		Setup: "1. In Microsoft Entra, register an app and create a client secret.\n" +
			"2. Give the app the Microsoft Graph application permission Calendars.ReadWrite. " +
			"Granted in Entra, it reaches every mailbox in your tenant, so limit it to the agents' mailboxes.\n" +
			"3. Best, use Exchange Online's RBAC for Applications: create a management scope for the agents' mailboxes and " +
			"assign the app the role Application Calendars.ReadWrite in that scope, without granting the permission in Entra.\n" +
			"4. Or grant it in Entra with admin consent, and restrict the app with an application access policy " +
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
	return a.box.Target(ctx, conn, target, "/calendar", "Calendars.ReadWrite")
}
