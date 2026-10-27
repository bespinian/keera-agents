// Package teams connects agents to Microsoft Teams through an Azure Bot:
// Bot Framework posts each activity to the webhook, and replies go back
// through the Bot Connector at the activity's service URL.
//
// Endpoints:
//   - "openid": Bot Framework's OpenID configuration, default
//     https://login.botframework.com/v1/.well-known/openidconfiguration
//   - "login": Microsoft Entra, default https://login.microsoftonline.com
//   - "serviceurl": one more service URL base to accept besides
//     Microsoft's hosts, and where messages no one asked for go, for tests
package teams

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/jwtverify"
	"github.com/bespinian/keera-agents/internal/channels/internal/microsoft"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

const (
	issuer = "https://api.botframework.com"
	scope  = "https://api.botframework.com/.default"
	// leeway is the clock skew Bot Framework's own SDKs allow.
	leeway = 5 * time.Minute
	// defaultTenant issues tokens for multi-tenant bots.
	defaultTenant = "botframework.com"
	lookupTimeout = 5 * time.Second
	lookupTTL     = 10 * time.Minute
	// globalServiceURL is where a message goes that answers no activity,
	// such as a schedule's result: only replies know their service URL.
	globalServiceURL = "https://smba.trafficmanager.net/teams/"
)

// Adapter is the Microsoft Teams chat adapter.
type Adapter struct {
	keys   *jwtverify.KeySet
	auth   *microsoft.Auth
	extra  string
	global string
	emails *web.Cache[string]
}

func New(opts channels.Options) *Adapter {
	hc := web.Client(opts)
	return &Adapter{
		keys:   jwtverify.NewOpenID(hc, web.Endpoint(opts, "openid", "https://login.botframework.com/v1/.well-known/openidconfiguration")),
		auth:   microsoft.NewAuth(opts),
		extra:  opts.Endpoints["serviceurl"],
		global: cmp.Or(opts.Endpoints["serviceurl"], globalServiceURL),
		emails: web.NewCache[string](lookupTTL, 10000),
	}
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:     channels.Teams,
		Label:    "Microsoft Teams",
		Vendor:   "Microsoft",
		Category: "Chat",
		Summary:  "Agents answer when mentioned in channels and in chats.",
		Fields: []channels.Field{
			{
				Name: "app_id", Label: "App ID", Required: true,
				Help: "The Microsoft App ID of the Azure Bot, under Configuration.",
			},
			{
				Name: "tenant_id", Label: "Tenant ID",
				Help: "Your Microsoft Entra tenant ID, for a single-tenant bot. Leave empty for a multi-tenant bot.",
			},
			{
				Name: "allowed_tenants", Label: "Other tenants",
				Help: "For a multi-tenant bot: the Entra tenant IDs whose people may message it, separated by commas. " +
					"Messages from any other tenant are refused.",
			},
			{
				Name: "app_password", Label: "App password", Secret: true, Required: true,
				Help: "A client secret of the bot's app registration, from Certificates & secrets.",
			},
		},
		Setup: "1. In the Azure portal, create an Azure Bot (single tenant is recommended) and note its Microsoft App ID.\n" +
			"2. In its app registration, create a client secret under Certificates & secrets.\n" +
			"3. In the bot's Configuration, set the messaging endpoint to {webhook}.\n" +
			"4. Under Channels, add Microsoft Teams.\n" +
			"5. Add the bot to Teams with an app package (for example in the Teams Developer Portal) " +
			"and install it for the people and teams that should use it.",
		TargetLabel: "Conversation ID",
		TargetHelp:  "A channel's ID, such as 19:…@thread.tacv2, or * for every chat and channel the bot is in.",
	}
}

type account struct {
	ID string `json:"id"`
}

type activity struct {
	Type         string  `json:"type"`
	ID           string  `json:"id"`
	ServiceURL   string  `json:"serviceUrl"`
	Text         string  `json:"text"`
	From         account `json:"from"`
	Recipient    account `json:"recipient"`
	Conversation struct {
		ID               string `json:"id"`
		TenantID         string `json:"tenantId"`
		ConversationType string `json:"conversationType"`
		Name             string `json:"name"`
		IsGroup          bool   `json:"isGroup"`
	} `json:"conversation"`
	Entities []struct {
		Type      string  `json:"type"`
		Text      string  `json:"text"`
		Mentioned account `json:"mentioned"`
	} `json:"entities"`
	ChannelData struct {
		Tenant  struct{ ID string }   `json:"tenant"`
		Team    struct{ Name string } `json:"team"`
		Channel struct{ Name string } `json:"channel"`
	} `json:"channelData"`
}

func (a *Adapter) Receive(ctx context.Context, conn channels.Connection, r *http.Request, body []byte) (channels.Receipt, error) {
	appID := web.Value(conn, "app_id")
	token := jwtverify.Bearer(r)
	if appID == "" || token == "" {
		return channels.Receipt{}, channels.ErrUnverified
	}
	var claims struct {
		ServiceURL string `json:"serviceurl"`
	}
	err := jwtverify.Verify(ctx, a.keys, token, jwtverify.Expected{
		Issuers: []string{issuer}, Audience: appID, Leeway: leeway,
	}, &claims)
	if err != nil {
		return channels.Receipt{}, fmt.Errorf("%w: %v", channels.ErrUnverified, err)
	}
	var act activity
	if err := json.Unmarshal(body, &act); err != nil {
		return channels.Receipt{}, fmt.Errorf("teams: reading the activity: %w", err)
	}
	// The token names the service URL it was issued for; an activity that
	// points elsewhere would have us send the bot's token there.
	if claims.ServiceURL != "" && !sameURL(claims.ServiceURL, act.ServiceURL) {
		return channels.Receipt{}, fmt.Errorf("%w: the service URL does not match the token", channels.ErrUnverified)
	}
	if act.Type != "message" {
		return channels.Receipt{}, nil
	}
	if !a.allowed(act.ServiceURL) {
		return channels.Receipt{}, fmt.Errorf("%w: the service URL is not a Microsoft address", channels.ErrUnverified)
	}
	// Anyone can add a bot to their own tenant, and set their account's
	// email there to whatever they like. So only the business's tenants are
	// trusted to say who someone is.
	if !fromTenant(act, tenants(conn)) {
		return channels.Receipt{}, fmt.Errorf("%w: the message comes from a tenant the connector does not allow", channels.ErrUnverified)
	}
	in, ok := parse(act, appID)
	if !ok {
		return channels.Receipt{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	in.Sender.Email = a.email(ctx, conn, in, act.From.ID)
	return channels.Receipt{Messages: []channels.Inbound{in}}, nil
}

var atTag = regexp.MustCompile(`(?s)<at>(.*?)</at>`)

func parse(act activity, appID string) (channels.Inbound, bool) {
	ct := act.Conversation.ConversationType
	direct := ct == "personal" || (ct == "" && !act.Conversation.IsGroup)
	text := act.Text
	mentioned := false
	for _, e := range act.Entities {
		if e.Type == "mention" && (e.Mentioned.ID == act.Recipient.ID || e.Mentioned.ID == "28:"+appID) {
			mentioned = true
			if e.Text != "" {
				text = strings.ReplaceAll(text, e.Text, "")
			}
		}
	}
	if !direct && !mentioned {
		return channels.Inbound{}, false
	}
	// Other people's mentions keep their names.
	text = strings.TrimSpace(html.UnescapeString(atTag.ReplaceAllString(text, "$1")))
	if text == "" || act.Conversation.ID == "" {
		return channels.Inbound{}, false
	}
	conv, root, _ := strings.Cut(act.Conversation.ID, ";messageid=")
	in := channels.Inbound{
		Target: channels.Target{
			Conversation: conv,
			Extra:        map[string]string{"serviceUrl": act.ServiceURL},
		},
		Text:      text,
		MessageID: conv + "/" + act.ID,
		Direct:    direct,
	}
	switch {
	case ct == "channel":
		// A post in a channel without a messageid is the thread's root.
		in.Target.Thread = cmp.Or(root, act.ID)
		channel := cmp.Or(act.ChannelData.Channel.Name, "General")
		in.ConversationLabel = strings.TrimPrefix(act.ChannelData.Team.Name+" / "+channel, " / ")
	case !direct:
		in.ConversationLabel = act.Conversation.Name
	}
	return in, true
}

// email asks the Bot Connector for the sender's email, which activities
// leave out.
func (a *Adapter) email(ctx context.Context, conn channels.Connection, in channels.Inbound, user string) string {
	key := conn.ID + "/" + user
	if e, ok := a.emails.Get(key, time.Now()); ok {
		return e
	}
	u := strings.TrimRight(in.Target.Extra["serviceUrl"], "/") + "/v3/conversations/" + component(in.Target.Conversation) +
		"/members/" + component(user)
	var m struct {
		Email             string `json:"email"`
		UserPrincipalName string `json:"userPrincipalName"`
	}
	if err := a.do(ctx, conn, http.MethodGet, u, nil, &m); err != nil {
		return ""
	}
	e := cmp.Or(m.Email, m.UserPrincipalName)
	a.emails.Put(key, e, time.Now())
	return e
}

func (a *Adapter) Send(ctx context.Context, conn channels.Connection, to channels.Target, text string) (string, error) {
	serviceURL := cmp.Or(to.Extra["serviceUrl"], a.global)
	if !a.allowed(serviceURL) {
		return "", errors.New("teams: the reply has no Microsoft service URL")
	}
	if to.Conversation == "" || to.Conversation == channels.AnyTarget {
		return "", errors.New("teams: no conversation to post to")
	}
	msg := map[string]any{"type": "message", "text": text, "textFormat": "markdown"}
	u := strings.TrimRight(serviceURL, "/") + "/v3/conversations/"
	if to.Thread != "" {
		// A reply in a channel thread goes to the thread's conversation, as
		// a reply to its root post.
		u += component(to.Conversation+";messageid="+to.Thread) + "/activities/" + component(to.Thread)
		msg["replyToId"] = to.Thread
	} else {
		u += component(to.Conversation) + "/activities"
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := a.do(ctx, conn, http.MethodPost, u, msg, &r); err != nil {
		return "", fmt.Errorf("teams: posting the message: %w", err)
	}
	return r.ID, nil
}

// do calls the Bot Connector with the bot's token.
func (a *Adapter) do(ctx context.Context, conn channels.Connection, method, u string, in, out any) error {
	app, err := credentials(conn)
	if err != nil {
		return err
	}
	req, err := web.NewJSONRequest(ctx, method, u, in)
	if err != nil {
		return err
	}
	return a.auth.DoJSON(req, app, "the Bot Connector", 1<<20, out)
}

func credentials(conn channels.Connection) (microsoft.App, error) {
	app := microsoft.App{
		Tenant:   cmp.Or(web.Value(conn, "tenant_id"), defaultTenant),
		ClientID: web.Value(conn, "app_id"),
		Secret:   web.Value(conn, "app_password"),
		Scope:    scope,
	}
	if app.ClientID == "" || app.Secret == "" {
		return app, errors.New("the app ID or app password is missing")
	}
	return app, nil
}

// tenants lists the tenants whose people may message the bot.
func tenants(conn channels.Connection) []string {
	var out []string
	if t := web.Value(conn, "tenant_id"); t != "" {
		out = append(out, t)
	}
	for t := range strings.SplitSeq(web.Value(conn, "allowed_tenants"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// fromTenant says whether an activity names a tenant, and only allowed ones.
// Teams puts it in two places; both must agree.
func fromTenant(act activity, allowed []string) bool {
	named := false
	for _, t := range []string{act.ChannelData.Tenant.ID, act.Conversation.TenantID} {
		if t == "" {
			continue
		}
		if !slices.ContainsFunc(allowed, func(a string) bool { return strings.EqualFold(a, t) }) {
			return false
		}
		named = true
	}
	return named
}

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	app, err := credentials(conn)
	if err != nil {
		return err
	}
	if len(tenants(conn)) == 0 {
		return errors.New("a multi-tenant bot needs the tenants whose people may message it, under Other tenants")
	}
	return a.auth.Check(ctx, app)
}

// allowed says whether a service URL is one of Microsoft's, so the bot's
// token is never sent anywhere else.
func (a *Adapter) allowed(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.Host == "" {
		return false
	}
	if a.extra != "" {
		if e, err := url.Parse(a.extra); err == nil && e.Scheme == u.Scheme && e.Host == u.Host {
			return true
		}
	}
	if u.Scheme != "https" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "smba.trafficmanager.net" {
		return true
	}
	for _, suffix := range []string{".botframework.com", ".botframework.us", ".botframework.azure.us", ".teams.microsoft.com", ".teams.microsoft.us"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func sameURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}

// component escapes a path segment as Bot Framework's SDKs do, since
// conversation ids hold characters such as ':' and ';'.
func component(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
