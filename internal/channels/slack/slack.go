// Package slack connects agents to Slack through a Slack app: messages
// arrive through the Events API and replies go out with chat.postMessage.
//
// Endpoints:
//   - "api": the Web API base, default https://slack.com/api
package slack

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/markup"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

const (
	// maxAge is how old a signed request may be, against replays. Slack
	// recommends five minutes.
	maxAge = 5 * time.Minute
	// lookupTimeout bounds the user and channel lookups in Receive: Slack
	// wants its answer within three seconds, or it retries.
	lookupTimeout = 2 * time.Second
	lookupTTL     = 10 * time.Minute
)

// Adapter is the Slack chat adapter.
type Adapter struct {
	hc     *http.Client
	api    string
	people *web.Cache[channels.Person]
	labels *web.Cache[string]
}

func New(opts channels.Options) *Adapter {
	return &Adapter{
		hc:     web.Client(opts),
		api:    web.Endpoint(opts, "api", "https://slack.com/api"),
		people: web.NewCache[channels.Person](lookupTTL, 10000),
		labels: web.NewCache[string](lookupTTL, 10000),
	}
}

func (a *Adapter) Descriptor() channels.Descriptor {
	return channels.Descriptor{
		Kind:  channels.Slack,
		Label: "Slack",
		Chat:  true,
		Fields: []channels.Field{
			{
				Name: "bot_token", Label: "Bot token", Secret: true, Required: true,
				Help: "The Bot User OAuth Token from OAuth & Permissions, starting with xoxb-.",
			},
			{
				Name: "signing_secret", Label: "Signing secret", Secret: true, Required: true,
				Help: "From Basic Information, under App Credentials. It proves events come from Slack.",
			},
		},
		Setup: "1. Create a Slack app at api.slack.com/apps and install it to your workspace.\n" +
			"2. Under OAuth & Permissions, add the bot scopes app_mentions:read, chat:write, im:history, users:read and users:read.email " +
			"(and channels:read, groups:read and im:read to show channel names).\n" +
			"3. Under Event Subscriptions, turn events on, set the request URL to {webhook}, and subscribe to the bot events app_mention and message.im.\n" +
			"4. Under App Home, allow users to send messages in the Messages tab.\n" +
			"5. Invite the bot to the channels it should answer in.",
		TargetLabel: "Channel ID",
		TargetHelp:  "C0123…, or * for every channel the bot is in and its direct messages.",
	}
}

// result is the envelope every Web API answer has.
type result struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// call calls a Web API method: a GET with query when body is nil, a JSON
// POST otherwise. A Slack error comes back as *apiError.
func (a *Adapter) call(ctx context.Context, token, method string, query url.Values, body, out any) error {
	u := a.api + "/" + method
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	verb := http.MethodGet
	if body != nil {
		verb = http.MethodPost
	}
	req, err := web.NewJSONRequest(ctx, verb, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := web.Call(a.hc, req)
	if err != nil {
		return fmt.Errorf("could not reach Slack: %w", err)
	}
	b, err := web.ReadBody(resp, 4<<20)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &apiError{Method: method, Code: resp.Status}
	}
	var r result
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("slack: %s: unreadable answer: %w", method, err)
	}
	if !r.OK {
		return &apiError{Method: method, Code: r.Error}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type apiError struct {
	Method string
	Code   string
}

func (e *apiError) Error() string { return "slack: " + e.Method + ": " + e.Code }

func (a *Adapter) Check(ctx context.Context, conn channels.Connection) error {
	token := web.Value(conn, "bot_token")
	switch {
	case token == "":
		return errors.New("the bot token is missing")
	case !strings.HasPrefix(token, "xoxb-"):
		return errors.New("the bot token must be a Bot User OAuth Token, starting with xoxb-")
	case web.Value(conn, "signing_secret") == "":
		return errors.New("the signing secret is missing")
	}
	var r struct {
		Team string `json:"team"`
		User string `json:"user"`
	}
	err := a.call(ctx, token, "auth.test", nil, struct{}{}, &r)
	if ae, ok := errors.AsType[*apiError](err); ok {
		// These are sentences for the administrator, so they start with
		// the service's name.
		switch ae.Code {
		case "invalid_auth", "not_authed":
			return fmt.Errorf("Slack says the bot token is invalid (%s)", ae.Code) //nolint:staticcheck
		case "token_revoked", "token_expired":
			return fmt.Errorf("Slack says the bot token is no longer valid; reinstall the app (%s)", ae.Code) //nolint:staticcheck
		case "account_inactive":
			return fmt.Errorf("Slack says the app was uninstalled or its account deactivated (%s)", ae.Code) //nolint:staticcheck
		default:
			return fmt.Errorf("Slack refused the bot token (%s)", ae.Code) //nolint:staticcheck
		}
	}
	return err
}

type envelope struct {
	Type           string `json:"type"`
	Challenge      string `json:"challenge"`
	EventID        string `json:"event_id"`
	TeamID         string `json:"team_id"`
	EnterpriseID   string `json:"enterprise_id"`
	Event          event  `json:"event"`
	Authorizations []struct {
		UserID       string `json:"user_id"`
		IsBot        bool   `json:"is_bot"`
		TeamID       string `json:"team_id"`
		EnterpriseID string `json:"enterprise_id"`
	} `json:"authorizations"`
}

// workspace is the Slack workspace the app is installed in, and its
// Enterprise Grid organisation if it has one.
type workspace struct{ team, enterprise string }

func workspaceOf(env envelope) workspace {
	w := workspace{team: env.TeamID, enterprise: env.EnterpriseID}
	for _, au := range env.Authorizations {
		if au.IsBot {
			w.team = cmp.Or(au.TeamID, w.team)
			w.enterprise = cmp.Or(au.EnterpriseID, w.enterprise)
			break
		}
	}
	return w
}

type event struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
}

func (a *Adapter) Receive(ctx context.Context, conn channels.Connection, r *http.Request, body []byte) (channels.Receipt, error) {
	if !a.verify(web.Value(conn, "signing_secret"), r.Header, body) {
		return channels.Receipt{}, channels.ErrUnverified
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return channels.Receipt{}, fmt.Errorf("slack: reading the event: %w", err)
	}
	switch env.Type {
	case "url_verification":
		b, _ := json.Marshal(map[string]string{"challenge": env.Challenge})
		return channels.Receipt{Response: channels.Response{Status: http.StatusOK, ContentType: "application/json", Body: b}}, nil
	case "event_callback":
	default:
		return channels.Receipt{}, nil
	}
	in, ok := parse(env)
	if !ok {
		return channels.Receipt{}, nil
	}
	a.lookup(ctx, conn, &in, env.Event.User, workspaceOf(env))
	return channels.Receipt{Messages: []channels.Inbound{in}}, nil
}

// verify checks Slack's v0 signature, an HMAC of the timestamp and body.
func (a *Adapter) verify(secret string, h http.Header, body []byte) bool {
	ts := h.Get("X-Slack-Request-Timestamp")
	sig := h.Get("X-Slack-Signature")
	if secret == "" || ts == "" || !strings.HasPrefix(sig, "v0=") {
		return false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if age := time.Since(time.Unix(sec, 0)); age > maxAge || age < -maxAge {
		return false
	}
	got, err := hex.DecodeString(sig[3:])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

var (
	mention  = regexp.MustCompile(`<@([A-Z0-9]+)(\|[^>]*)?>`)
	entities = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")
)

// parse picks the messages meant for the bot: mentions in channels and
// anything in its direct messages, never its own or another bot's.
func parse(env envelope) (channels.Inbound, bool) {
	ev := env.Event
	var botUser string
	for _, au := range env.Authorizations {
		if au.IsBot {
			botUser = au.UserID
			break
		}
	}
	direct := ev.ChannelType == "im" || strings.HasPrefix(ev.Channel, "D")
	switch {
	case ev.Type == "app_mention":
	case ev.Type == "message" && ev.ChannelType == "im":
	default:
		return channels.Inbound{}, false
	}
	// Edits, deletions, joins and bot posts arrive as subtypes. A file with
	// a caption and a reply also sent to the channel are still a person
	// talking to the bot.
	if ev.Subtype != "" && ev.Subtype != "file_share" && ev.Subtype != "thread_broadcast" {
		return channels.Inbound{}, false
	}
	if ev.BotID != "" || ev.User == "" || (botUser != "" && ev.User == botUser) || ev.Channel == "" {
		return channels.Inbound{}, false
	}
	text := mention.ReplaceAllStringFunc(ev.Text, func(m string) string {
		id := mention.FindStringSubmatch(m)[1]
		if id == botUser || (botUser == "" && ev.Type == "app_mention" && strings.HasPrefix(strings.TrimSpace(ev.Text), m)) {
			return ""
		}
		return m
	})
	text = strings.TrimSpace(entities.Replace(text))
	if text == "" {
		return channels.Inbound{}, false
	}
	// Replies in a channel go in the thread, starting one if need be. In a
	// direct message a top-level reply reads more naturally.
	thread := ev.ThreadTS
	if thread == "" && !direct {
		thread = ev.TS
	}
	// channel+ts names the message itself, so the app_mention and message
	// events Slack can send for one post collapse into one, as do retries.
	id := env.EventID
	if ev.TS != "" {
		id = ev.Channel + "/" + ev.TS
	}
	return channels.Inbound{
		Target:    channels.Target{Conversation: ev.Channel, Thread: thread},
		Text:      text,
		MessageID: id,
		Direct:    direct,
	}, true
}

// lookup fills in the sender's email and the channel's name. Failures leave
// them empty: without an email the message is refused upstream, which is
// the right outcome when Slack will not say who sent it.
func (a *Adapter) lookup(ctx context.Context, conn channels.Connection, in *channels.Inbound, user string, ws workspace) {
	token := web.Value(conn, "bot_token")
	if token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { in.Sender = a.person(ctx, conn.ID, token, user, ws) })
	if !in.Direct {
		wg.Go(func() { in.ConversationLabel = a.channelName(ctx, conn.ID, token, in.Target.Conversation) })
	}
	wg.Wait()
}

// person looks up who sent a message. Someone from another workspace, as in
// a Slack Connect channel, is marked Outside: their workspace's admins set
// their email, so it says nothing about who they are here.
func (a *Adapter) person(ctx context.Context, connID, token, user string, ws workspace) channels.Person {
	key := connID + "/" + user
	if p, ok := a.people.Get(key, time.Now()); ok {
		return p
	}
	var r struct {
		User struct {
			TeamID         string `json:"team_id"`
			EnterpriseUser struct {
				EnterpriseID string `json:"enterprise_id"`
			} `json:"enterprise_user"`
			Profile struct {
				Email string `json:"email"`
			} `json:"profile"`
		} `json:"user"`
	}
	if a.call(ctx, token, "users.info", url.Values{"user": {user}}, nil, &r) != nil {
		return channels.Person{}
	}
	u := r.User
	inside := (ws.team != "" && u.TeamID == ws.team) ||
		(ws.enterprise != "" && (u.EnterpriseUser.EnterpriseID == ws.enterprise || u.TeamID == ws.enterprise))
	p := channels.Person{Email: u.Profile.Email}
	if !inside {
		p = channels.Person{Outside: true}
	}
	a.people.Put(key, p, time.Now())
	return p
}

func (a *Adapter) channelName(ctx context.Context, connID, token, channel string) string {
	key := connID + "/" + channel
	if l, ok := a.labels.Get(key, time.Now()); ok {
		return l
	}
	var r struct {
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	}
	if a.call(ctx, token, "conversations.info", url.Values{"channel": {channel}}, nil, &r) != nil || r.Channel.Name == "" {
		return ""
	}
	l := "#" + r.Channel.Name
	a.labels.Put(key, l, time.Now())
	return l
}

func (a *Adapter) Send(ctx context.Context, conn channels.Connection, to channels.Target, text string) (string, error) {
	token := web.Value(conn, "bot_token")
	if token == "" {
		return "", errors.New("slack: the bot token is missing")
	}
	if to.Conversation == "" || to.Conversation == "*" {
		return "", errors.New("slack: no channel to post to")
	}
	msg := map[string]any{
		"channel": to.Conversation,
		"text":    markup.Mrkdwn(text, true),
		"mrkdwn":  true,
	}
	if to.Thread != "" {
		msg["thread_ts"] = to.Thread
	}
	var r struct {
		TS string `json:"ts"`
	}
	if err := a.call(ctx, token, "chat.postMessage", nil, msg, &r); err != nil {
		return "", err
	}
	return r.TS, nil
}
