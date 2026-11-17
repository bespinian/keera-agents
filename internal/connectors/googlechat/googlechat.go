// Package googlechat connects agents to Google Chat through a Chat app with
// an HTTP endpoint. Chat posts each event to the webhook, which answers at
// once; the agent's reply goes out later through the Chat API, signed in
// as the app's service account.
//
// Both event shapes are understood: the classic Chat app event ("type":
// "MESSAGE") and the Google Workspace add-on event ("chat.messagePayload").
//
// Endpoints:
//   - "token": Google's token endpoint, default https://oauth2.googleapis.com/token
//   - "chat": the Chat API base, default https://chat.googleapis.com
//   - "chatkeys": the keys Chat signs project-number tokens with, default
//     https://www.googleapis.com/service_accounts/v1/metadata/jwk/chat@system.gserviceaccount.com
//   - "googlekeys": Google's OpenID Connect keys, default
//     https://www.googleapis.com/oauth2/v3/certs
package googlechat

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/google"
	"github.com/bespinian/keera-agents/internal/connectors/internal/jwtverify"
	"github.com/bespinian/keera-agents/internal/connectors/internal/markup"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

const (
	scope      = "https://www.googleapis.com/auth/chat.bot"
	chatSender = "chat@system.gserviceaccount.com"
	leeway     = 2 * time.Minute
	hello      = "Hello! Mention me in a message, or send me a direct message, and I will get to work."
)

var (
	projectNumber = regexp.MustCompile(`^[0-9]+$`)
	// addOnSender is the service account Google signs Workspace add-on
	// events with, one per project.
	addOnSender = regexp.MustCompile(`^service-[0-9]+@gcp-sa-gsuiteaddons\.iam\.gserviceaccount\.com$`)
	spaceName   = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+$`)
)

// Adapter is the Google Chat adapter.
type Adapter struct {
	auth       *google.Auth
	chat       string
	chatKeys   *jwtverify.KeySet
	googleKeys *jwtverify.KeySet
}

func New(opts connectors.Options) *Adapter {
	hc := web.Client(opts)
	return &Adapter{
		auth: google.NewAuth(opts),
		chat: web.Endpoint(opts, "chat", "https://chat.googleapis.com"),
		chatKeys: jwtverify.NewJWKS(hc, web.Endpoint(opts, "chatkeys",
			"https://www.googleapis.com/service_accounts/v1/metadata/jwk/"+chatSender)),
		googleKeys: jwtverify.NewJWKS(hc, web.Endpoint(opts, "googlekeys", "https://www.googleapis.com/oauth2/v3/certs")),
	}
}

func (a *Adapter) Descriptor() connectors.Descriptor {
	return connectors.Descriptor{
		Kind:     connectors.GoogleChat,
		Label:    "Google Chat",
		Vendor:   "Google",
		Category: "Chat",
		Summary:  "Agents answer when mentioned in spaces and in direct messages.",
		Fields: []connectors.Field{
			{
				Name: "service_account_json", Label: "Service account key", Secret: true, Required: true, Multiline: true,
				Help: "The JSON key file of the service account the Chat app sends replies as.",
			},
			{
				Name: "audience", Label: "Authentication audience", Required: true,
				Help: "What you chose as the Chat app's authentication audience: the project number " +
					"(recommended), or the HTTP endpoint URL.",
			},
		},
		Setup: "1. In a Google Cloud project, enable the Google Chat API.\n" +
			"2. Create a service account with a JSON key.\n" +
			"3. In the Chat API's Configuration, give the app a name and turn on 1:1 messages and joining spaces.\n" +
			"4. Choose HTTP endpoint URL, enter {webhook}, and set the authentication audience to the project number " +
			"(found on the project's dashboard).\n" +
			"5. Choose who in your domain can use the app.",
		TargetLabel: "Space",
		TargetHelp:  "A space's resource name, such as spaces/AAAA…, or * for every space and direct message.",
	}
}

func (a *Adapter) Check(ctx context.Context, conn connectors.Connection) error {
	if err := checkAudience(web.Value(conn, "audience")); err != nil {
		return err
	}
	return a.auth.Check(ctx, google.Creds{Key: web.Value(conn, "service_account_json")}, scope)
}

func checkAudience(aud string) error {
	switch {
	case aud == "":
		return errors.New("the authentication audience is missing")
	case projectNumber.MatchString(aud):
		return nil
	case strings.HasPrefix(aud, "https://"):
		return nil
	default:
		return errors.New("the authentication audience must be the project number or the https:// endpoint URL")
	}
}

type user struct {
	Email string `json:"email"`
	Type  string `json:"type"`
}

type space struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	SpaceType       string `json:"spaceType"`
	DisplayName     string `json:"displayName"`
	SingleUserBotDM bool   `json:"singleUserBotDm"`
}

type message struct {
	Name         string `json:"name"`
	Text         string `json:"text"`
	ArgumentText string `json:"argumentText"`
	Sender       user   `json:"sender"`
	Thread       struct {
		Name string `json:"name"`
	} `json:"thread"`
	Annotations []struct {
		Type        string `json:"type"`
		UserMention *struct {
			User user `json:"user"`
		} `json:"userMention"`
	} `json:"annotations"`
	SlashCommand json.RawMessage `json:"slashCommand"`
}

type event struct {
	Type    string  `json:"type"`
	Space   space   `json:"space"`
	Message message `json:"message"`
	User    user    `json:"user"`
	// Chat is set on Workspace add-on events instead of the fields above.
	Chat *struct {
		User           user `json:"user"`
		MessagePayload *struct {
			Space   space   `json:"space"`
			Message message `json:"message"`
		} `json:"messagePayload"`
		AddedToSpacePayload *struct {
			Space space `json:"space"`
		} `json:"addedToSpacePayload"`
	} `json:"chat"`
}

func (a *Adapter) Receive(ctx context.Context, conn connectors.Connection, r *http.Request, body []byte) (connectors.Receipt, error) {
	if err := a.verify(ctx, web.Value(conn, "audience"), jwtverify.Bearer(r)); err != nil {
		return connectors.Receipt{}, fmt.Errorf("%w: %v", connectors.ErrUnverified, err)
	}
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		return connectors.Receipt{}, fmt.Errorf("googlechat: reading the event: %w", err)
	}
	addOn := ev.Chat != nil
	if addOn {
		ev.User = ev.Chat.User
		switch {
		case ev.Chat.MessagePayload != nil:
			ev.Type, ev.Space, ev.Message = "MESSAGE", ev.Chat.MessagePayload.Space, ev.Chat.MessagePayload.Message
		case ev.Chat.AddedToSpacePayload != nil:
			ev.Type, ev.Space = "ADDED_TO_SPACE", ev.Chat.AddedToSpacePayload.Space
		}
	}
	out := connectors.Receipt{Response: empty()}
	switch ev.Type {
	case "ADDED_TO_SPACE":
		out.Response = reply(hello, addOn)
	case "MESSAGE":
		if in, ok := parse(ev); ok {
			out.Messages = []connectors.Inbound{in}
		}
	}
	return out, nil
}

// verify checks the bearer token. With a project number as audience, Chat
// signs it as chat@system.gserviceaccount.com; with the endpoint URL, it
// is a Google ID token for that service account, or for the project's
// add-on service account.
func (a *Adapter) verify(ctx context.Context, aud, token string) error {
	if token == "" || checkAudience(aud) != nil {
		return errors.New("no token, or no audience to check it against")
	}
	if projectNumber.MatchString(aud) {
		return jwtverify.Verify(ctx, a.chatKeys, token, jwtverify.Expected{
			Issuers: []string{chatSender}, Audience: aud, Leeway: leeway,
		}, nil)
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	err := jwtverify.Verify(ctx, a.googleKeys, token, jwtverify.Expected{
		Issuers: []string{"https://accounts.google.com", "accounts.google.com"}, Audience: aud, Leeway: leeway,
	}, &claims)
	if err != nil {
		return err
	}
	if !claims.EmailVerified || (claims.Email != chatSender && !addOnSender.MatchString(claims.Email)) {
		return fmt.Errorf("the token is for %q, not Google Chat", web.Short(claims.Email, 80))
	}
	return nil
}

func parse(ev event) (connectors.Inbound, bool) {
	m := ev.Message
	direct := ev.Space.Type == "DM" || ev.Space.SpaceType == "DIRECT_MESSAGE" || ev.Space.SingleUserBotDM
	mentioned := len(m.SlashCommand) > 0
	for _, an := range m.Annotations {
		if an.Type == "USER_MENTION" && an.UserMention != nil && an.UserMention.User.Type == "BOT" {
			mentioned = true
		}
	}
	if !direct && !mentioned {
		return connectors.Inbound{}, false
	}
	// argumentText is the text without the app's mention.
	text := strings.TrimSpace(m.ArgumentText)
	if text == "" {
		text = strings.TrimSpace(m.Text)
	}
	if text == "" || ev.Space.Name == "" {
		return connectors.Inbound{}, false
	}
	in := connectors.Inbound{
		Target:    connectors.Target{Conversation: ev.Space.Name, Thread: m.Thread.Name},
		Sender:    connectors.Person{Email: cmp.Or(m.Sender.Email, ev.User.Email)},
		Text:      text,
		MessageID: cmp.Or(m.Name, ev.Space.Name+"/"+m.Thread.Name+"/"+text),
		Direct:    direct,
	}
	if !direct {
		in.ConversationLabel = ev.Space.DisplayName
	}
	return in, true
}

func empty() connectors.Response {
	return connectors.Response{Status: http.StatusOK, ContentType: "application/json", Body: []byte("{}")}
}

// reply is a message sent as the webhook's answer, in the shape the event
// came in.
func reply(text string, addOn bool) connectors.Response {
	var v any = map[string]string{"text": text}
	if addOn {
		v = map[string]any{"hostAppDataAction": map[string]any{"chatDataAction": map[string]any{
			"createMessageAction": map[string]any{"message": v},
		}}}
	}
	b, _ := json.Marshal(v)
	return connectors.Response{Status: http.StatusOK, ContentType: "application/json", Body: b}
}

func (a *Adapter) Send(ctx context.Context, conn connectors.Connection, to connectors.Target, text string) (string, error) {
	if !spaceName.MatchString(to.Conversation) {
		return "", errors.New("googlechat: no space to post to")
	}
	msg := map[string]any{"text": markup.Mrkdwn(text, false)}
	if to.Thread != "" {
		if !strings.HasPrefix(to.Thread, to.Conversation+"/threads/") {
			return "", errors.New("googlechat: the thread is not in the space")
		}
		msg["thread"] = map[string]string{"name": to.Thread}
	}
	u := a.chat + "/v1/" + to.Conversation + "/messages?" + url.Values{
		"messageReplyOption": {"REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"},
	}.Encode()
	req, err := web.NewJSONRequest(ctx, http.MethodPost, u, msg)
	if err != nil {
		return "", err
	}
	var r struct {
		Name string `json:"name"`
	}
	if err := a.auth.DoJSON(req, google.Creds{Key: web.Value(conn, "service_account_json")}, scope, 1<<20, &r); err != nil {
		return "", fmt.Errorf("googlechat: posting the message: %w", err)
	}
	return r.Name, nil
}
