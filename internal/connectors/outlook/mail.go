package outlook

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/mailtext"
	"github.com/bespinian/keera-agents/internal/connectors/internal/microsoft"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

const (
	// maxBody bounds a message body read, in bytes, so one long thread
	// does not fill the model's context.
	maxBody       = 20000
	textBody      = `outlook.body-content-type="text"`
	messageFields = "id,from,toRecipients,subject,receivedDateTime,isRead,bodyPreview"
)

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{
			Name: "list_messages",
			Description: "List messages in a mail folder, newest first, with a short preview. " +
				"Search finds words in the subject, body and addresses.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"folder":{"type":"string","description":"A well-known folder (inbox, sentitems, drafts, archive, deleteditems, junkemail) or a folder id. Default inbox."},
				"search":{"type":"string","description":"Words to search for."},
				"unread_only":{"type":"boolean"},
				"limit":{"type":"integer","description":"At most 100. Default 25."}}}`),
			Run: run(a.listMessages),
		},
		{
			Name:        "list_folders",
			Description: "List the mailbox's top-level mail folders, with their ids and counts.",
			Parameters:  apitool.Schema(`{"type":"object","properties":{}}`),
			Run:         run(a.listFolders),
		},
		{
			Name:        "read_message",
			Description: "Read one message: its addresses, body as text and attachments' names.",
			Parameters: apitool.Schema(`{"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"The message's id, from list_messages."}}}`),
			Run: run(a.readMessage),
		},
		{
			Name:        "send_message",
			Description: "Send a new email from the mailbox. The body is sent as plain text.",
			Parameters: apitool.Schema(`{"type":"object","required":["to","subject","body"],"properties":{
				"to":{"type":"array","items":{"type":"string"},"description":"Email addresses."},
				"cc":{"type":"array","items":{"type":"string"}},
				"subject":{"type":"string"},
				"body":{"type":"string","description":"Plain text or simple Markdown."}}}`),
			Write: true,
			Run:   run(a.sendMessage),
		},
		{
			Name:        "reply",
			Description: "Reply to a message, above the quoted original.",
			Parameters: apitool.Schema(`{"type":"object","required":["id","body"],"properties":{
				"id":{"type":"string","description":"The message's id."},
				"body":{"type":"string"},
				"reply_all":{"type":"boolean","description":"Reply to everyone on the message, not only its sender."}}}`),
			Write: true,
			Run:   run(a.reply),
		},
		{
			Name:        "move_message",
			Description: "Move a message to another folder. Its id changes; the new one is returned.",
			Parameters: apitool.Schema(`{"type":"object","required":["id","folder"],"properties":{
				"id":{"type":"string","description":"The message's id."},
				"folder":{"type":"string","description":"A well-known folder (inbox, archive, deleteditems, junkemail) or a folder id from list_folders."}}}`),
			Write: true,
			Run:   run(a.moveMessage),
		},
		{
			Name:        "list_events",
			Description: "List calendar events in a time range, earliest first. Default the next 7 days.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"start":{"type":"string","description":"RFC 3339 time or a date, such as 2026-11-05. Default now."},
				"end":{"type":"string","description":"RFC 3339 time or a date. Default 7 days after start."},
				"time_zone":{"type":"string","description":"The time zone to show times in, such as Europe/Zurich. Default UTC."},
				"limit":{"type":"integer","description":"At most 100. Default 50."}}}`),
			Run: run(a.listEvents),
		},
		{
			Name:        "create_event",
			Description: "Create a calendar event. Attendees get an invitation by email.",
			Parameters: apitool.Schema(`{"type":"object","required":["subject","start","end"],"properties":{
				"subject":{"type":"string"},
				"start":{"type":"string","description":"Local time in time_zone, such as 2026-11-05T14:00, or RFC 3339 with an offset."},
				"end":{"type":"string","description":"As start."},
				"time_zone":{"type":"string","description":"Such as Europe/Zurich. Default UTC."},
				"attendees":{"type":"array","items":{"type":"string"},"description":"Email addresses to invite."},
				"body":{"type":"string","description":"Plain text."},
				"location":{"type":"string"},
				"online_meeting":{"type":"boolean","description":"Add a Teams meeting."}}}`),
			Write: true,
			Run:   run(a.createEvent),
		},
	}
}

type emailAddress struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

type recipient struct {
	EmailAddress emailAddress `json:"emailAddress"`
}

func (r recipient) String() string {
	if r.EmailAddress.Name == "" || r.EmailAddress.Name == r.EmailAddress.Address {
		return r.EmailAddress.Address
	}
	return r.EmailAddress.Name + " <" + r.EmailAddress.Address + ">"
}

func names(rs []recipient) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.String()
	}
	return out
}

type message struct {
	ID               string      `json:"id"`
	Subject          string      `json:"subject"`
	From             *recipient  `json:"from"`
	ToRecipients     []recipient `json:"toRecipients"`
	CcRecipients     []recipient `json:"ccRecipients"`
	ReceivedDateTime time.Time   `json:"receivedDateTime"`
	IsRead           bool        `json:"isRead"`
	BodyPreview      string      `json:"bodyPreview"`
	Body             struct {
		Content string `json:"content"`
	} `json:"body"`
	ConversationID string `json:"conversationId"`
	WebLink        string `json:"webLink"`
	Attachments    []struct {
		Name        string `json:"name"`
		Size        int64  `json:"size"`
		ContentType string `json:"contentType"`
	} `json:"attachments"`
}

// Brief is a message in a list.
type Brief struct {
	ID       string    `json:"id"`
	From     string    `json:"from,omitempty"`
	To       []string  `json:"to,omitempty"`
	Subject  string    `json:"subject"`
	Received time.Time `json:"received"`
	IsRead   bool      `json:"is_read"`
	Preview  string    `json:"preview,omitempty"`
}

func (m message) brief() Brief {
	b := Brief{
		ID: m.ID, To: names(m.ToRecipients), Subject: m.Subject,
		Received: m.ReceivedDateTime, IsRead: m.IsRead, Preview: m.BodyPreview,
	}
	if m.From != nil {
		b.From = m.From.String()
	}
	return b
}

// searchUnsafe is what would end the quoted $search early.
var searchUnsafe = strings.NewReplacer(`"`, " ", `\`, " ")

func (a *Adapter) listMessages(ctx context.Context, app microsoft.App, box string, in struct {
	Folder     string `json:"folder"`
	Search     string `json:"search"`
	UnreadOnly bool   `json:"unread_only"`
	Limit      int    `json:"limit"`
},
) (any, error) {
	if in.Folder == "" {
		in.Folder = "inbox"
	}
	folder, err := id("folder", in.Folder)
	if err != nil {
		return nil, err
	}
	limit := apitool.Limit(in.Limit, 25, 100)
	q := map[string]string{"$select": messageFields, "$top": strconv.Itoa(limit)}
	switch search := strings.TrimSpace(searchUnsafe.Replace(in.Search)); {
	case search != "":
		// Graph sorts search results itself and takes no $filter with
		// them, so unread is filtered here.
		q["$search"] = `"` + web.Cut(search, 200, "") + `"`
	case in.UnreadOnly:
		// Graph wants the $orderby property in the $filter, first.
		q["$filter"] = "receivedDateTime ge 1900-01-01T00:00:00Z and isRead eq false"
		q["$orderby"] = "receivedDateTime desc"
	default:
		q["$orderby"] = "receivedDateTime desc"
	}
	var r struct {
		Value []message `json:"value"`
	}
	if err := a.call(ctx, app, box, http.MethodGet, "/mailFolders/"+folder+"/messages", q, nil, &r); err != nil {
		return nil, err
	}
	out := []Brief{}
	for _, m := range r.Value {
		if !in.UnreadOnly || !m.IsRead {
			out = append(out, m.brief())
		}
	}
	return out, nil
}

// Folder is a mail folder.
type Folder struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Unread int    `json:"unread"`
	Total  int    `json:"total"`
}

func (a *Adapter) listFolders(ctx context.Context, app microsoft.App, box string, _ struct{}) (any, error) {
	var r struct {
		Value []struct {
			ID              string `json:"id"`
			DisplayName     string `json:"displayName"`
			UnreadItemCount int    `json:"unreadItemCount"`
			TotalItemCount  int    `json:"totalItemCount"`
		} `json:"value"`
	}
	q := map[string]string{"$select": "id,displayName,unreadItemCount,totalItemCount", "$top": "100"}
	if err := a.call(ctx, app, box, http.MethodGet, "/mailFolders", q, nil, &r); err != nil {
		return nil, err
	}
	out := make([]Folder, len(r.Value))
	for i, f := range r.Value {
		out[i] = Folder{ID: f.ID, Name: f.DisplayName, Unread: f.UnreadItemCount, Total: f.TotalItemCount}
	}
	return out, nil
}

// Message is one message read whole.
type Message struct {
	Brief
	Cc           []string     `json:"cc,omitempty"`
	Conversation string       `json:"conversation_id,omitempty"`
	Body         string       `json:"body"`
	Attachments  []Attachment `json:"attachments,omitempty"`
	WebLink      string       `json:"web_link,omitempty"`
}

type Attachment struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
	Size int64  `json:"size"`
}

func (a *Adapter) readMessage(ctx context.Context, app microsoft.App, box string, in struct {
	ID string `json:"id"`
},
) (any, error) {
	mid, err := id("message", in.ID)
	if err != nil {
		return nil, err
	}
	q := map[string]string{
		"$select": "id,from,toRecipients,ccRecipients,subject,receivedDateTime,isRead,body,conversationId,webLink",
		"$expand": "attachments($select=name,size,contentType)",
	}
	var m message
	if err := a.call(ctx, app, box, http.MethodGet, "/messages/"+mid, q, nil, &m, textBody); err != nil {
		return nil, err
	}
	out := Message{
		Brief: m.brief(), Cc: names(m.CcRecipients), Conversation: m.ConversationID,
		Body: mailtext.Body(m.Body.Content, maxBody), WebLink: m.WebLink,
	}
	out.Preview = ""
	for _, at := range m.Attachments {
		out.Attachments = append(out.Attachments, Attachment{Name: at.Name, Type: at.ContentType, Size: at.Size})
	}
	return out, nil
}

// recipients checks addresses the model gives, which may name the person
// too, such as "Ada <ada@example.com>".
func recipients(field string, list []string) ([]recipient, error) {
	addrs, err := mailtext.Addresses(field, list)
	out := make([]recipient, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, recipient{EmailAddress: emailAddress{Name: addr.Name, Address: addr.Address}})
	}
	return out, err
}

type body struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

func (a *Adapter) sendMessage(ctx context.Context, app microsoft.App, box string, in struct {
	To      []string `json:"to"`
	Cc      []string `json:"cc"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
},
) (any, error) {
	if len(in.To) == 0 {
		return nil, errors.New("give to, at least one address")
	}
	if err := apitool.Need("subject", in.Subject); err != nil {
		return nil, err
	}
	to, err := recipients("to", in.To)
	if err != nil {
		return nil, err
	}
	cc, err := recipients("cc", in.Cc)
	if err != nil {
		return nil, err
	}
	msg := map[string]any{
		"subject":      in.Subject,
		"body":         body{ContentType: "Text", Content: in.Body},
		"toRecipients": to,
	}
	if len(cc) > 0 {
		msg["ccRecipients"] = cc
	}
	req := map[string]any{"message": msg, "saveToSentItems": true}
	if err := a.call(ctx, app, box, http.MethodPost, "/sendMail", nil, req, nil); err != nil {
		return nil, err
	}
	return map[string]any{"sent": true, "to": names(to)}, nil
}

func (a *Adapter) reply(ctx context.Context, app microsoft.App, box string, in struct {
	ID       string `json:"id"`
	Body     string `json:"body"`
	ReplyAll bool   `json:"reply_all"`
},
) (any, error) {
	mid, err := id("message", in.ID)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("body", in.Body); err != nil {
		return nil, err
	}
	action := "/reply"
	if in.ReplyAll {
		action = "/replyAll"
	}
	// Graph puts the comment into the HTML reply above the quote, so the
	// text is escaped and its line breaks kept.
	comment := strings.ReplaceAll(html.EscapeString(in.Body), "\n", "<br>\n")
	if err := a.call(ctx, app, box, http.MethodPost, "/messages/"+mid+action, nil, map[string]string{"comment": comment}, nil); err != nil {
		return nil, err
	}
	return map[string]any{"sent": true, "reply_all": in.ReplyAll}, nil
}

func (a *Adapter) moveMessage(ctx context.Context, app microsoft.App, box string, in struct {
	ID     string `json:"id"`
	Folder string `json:"folder"`
},
) (any, error) {
	mid, err := id("message", in.ID)
	if err != nil {
		return nil, err
	}
	if _, err := id("folder", in.Folder); err != nil {
		return nil, err
	}
	var m message
	req := map[string]string{"destinationId": strings.TrimSpace(in.Folder)}
	if err := a.call(ctx, app, box, http.MethodPost, "/messages/"+mid+"/move", nil, req, &m); err != nil {
		return nil, err
	}
	return map[string]any{"moved": true, "id": m.ID, "folder": in.Folder}, nil
}
