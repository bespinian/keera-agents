package gmail

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/mailtext"
	"github.com/bespinian/keera-agents/internal/connectors/internal/workspace"
)

const (
	// maxBody bounds a message body read, in bytes, and maxThreadBody
	// each message of a thread, so one long thread does not fill the
	// model's context.
	maxBody       = 20000
	maxThreadBody = 4000
)

func (a *Adapter) toolSet() apitool.Set {
	recipients := `"to":{"type":"array","items":{"type":"string"},"description":"Email addresses, such as Ada <ada@example.com>."},
		"cc":{"type":"array","items":{"type":"string"}},
		"bcc":{"type":"array","items":{"type":"string"}},`
	return apitool.Set{
		{
			Name: "search_messages",
			Description: "Search messages, newest first, with a short preview. The query is Gmail's search syntax, such as " +
				"from:ada@example.com is:unread newer_than:7d label:invoices. Without one, it lists the inbox.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"query":{"type":"string"},
				"limit":{"type":"integer","description":"At most 50. Default 20."}}}`),
			Run: run(a.search),
		},
		{
			Name:        "read_message",
			Description: "Read one message: its addresses, its body as text and its attachments' names.",
			Parameters: apitool.Schema(`{"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"The message's id, from search_messages."}}}`),
			Run: run(a.readMessage),
		},
		{
			Name:        "read_thread",
			Description: "Read a whole conversation, oldest message first, each body cut short.",
			Parameters: apitool.Schema(`{"type":"object","required":["thread_id"],"properties":{
				"thread_id":{"type":"string","description":"The thread's id, from search_messages."}}}`),
			Run: run(a.readThread),
		},
		{
			Name:        "list_labels",
			Description: "List the mailbox's labels: the system ones, such as INBOX, UNREAD and STARRED, and its own.",
			Parameters:  apitool.Schema(`{"type":"object","properties":{}}`),
			Run:         run(a.listLabels),
		},
		{
			Name:        "send_message",
			Description: "Send a new email, as plain text.",
			Parameters: apitool.Schema(`{"type":"object","required":["to","subject","body"],"properties":{` + recipients + `
				"subject":{"type":"string"},
				"body":{"type":"string","description":"Plain text."}}}`),
			Write: true,
			Run:   run(a.send),
		},
		{
			Name:        "reply",
			Description: "Reply to a message, in its thread, above the quoted original.",
			Parameters: apitool.Schema(`{"type":"object","required":["id","body"],"properties":{
				"id":{"type":"string","description":"The message's id."},
				"body":{"type":"string","description":"Plain text."},
				"reply_all":{"type":"boolean","description":"Reply to everyone on the message, not only its sender."}}}`),
			Write: true,
			Run:   run(a.reply),
		},
		{
			Name: "create_draft",
			Description: "Save an email as a draft, for a person to check and send. Give reply_to to draft a reply " +
				"in that message's thread; to then defaults to its sender.",
			Parameters: apitool.Schema(`{"type":"object","required":["body"],"properties":{` + recipients + `
				"subject":{"type":"string","description":"Needed unless it replies."},
				"body":{"type":"string","description":"Plain text."},
				"reply_to":{"type":"string","description":"The id of the message it answers."}}}`),
			Write: true,
			Run:   run(a.draft),
		},
		{
			Name: "change_labels",
			Description: "Add labels to a message or remove them. Archive it by removing INBOX, mark it read by removing UNREAD, " +
				"star it by adding STARRED.",
			Parameters: apitool.Schema(`{"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"The message's id."},
				"add":{"type":"array","items":{"type":"string"},"description":"Label names or ids."},
				"remove":{"type":"array","items":{"type":"string"},"description":"Label names or ids."}}}`),
			Write: true,
			Run:   run(a.changeLabels),
		},
	}
}

// message is a message as Gmail answers it.
type message struct {
	ID       string   `json:"id"`
	ThreadID string   `json:"threadId"`
	LabelIDs []string `json:"labelIds"`
	Snippet  string   `json:"snippet"`
	Payload  part     `json:"payload"`
}

type part struct {
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Headers  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Body struct {
		Size int64  `json:"size"`
		Data string `json:"data"`
	} `json:"body"`
	Parts []part `json:"parts"`
}

// header is the value of the named header, or empty.
func (p part) header(name string) string {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// Brief is a message as search lists it.
type Brief struct {
	ID       string   `json:"id"`
	ThreadID string   `json:"thread_id"`
	From     string   `json:"from,omitempty"`
	To       string   `json:"to,omitempty"`
	Subject  string   `json:"subject,omitempty"`
	Date     string   `json:"date,omitempty"`
	Unread   bool     `json:"unread,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	Preview  string   `json:"preview,omitempty"`
}

func (m message) brief() Brief {
	return Brief{
		ID: m.ID, ThreadID: m.ThreadID, From: m.Payload.header("From"), To: m.Payload.header("To"),
		Subject: m.Payload.header("Subject"), Date: m.Payload.header("Date"),
		Unread: slices.Contains(m.LabelIDs, "UNREAD"), Labels: m.LabelIDs, Preview: html.UnescapeString(m.Snippet),
	}
}

// Message is a message as read_message and read_thread return it.
type Message struct {
	Brief
	Cc          string       `json:"cc,omitempty"`
	Body        string       `json:"body"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

type Attachment struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
	Size int64  `json:"size"`
}

// full reads a message with its body, cut to n characters.
func (m message) full(n int) Message {
	out := Message{Brief: m.brief(), Cc: m.Payload.header("Cc"), Body: mailtext.Body(text(m.Payload), n)}
	out.Preview, out.Labels = "", nil
	walk(m.Payload, func(p part) {
		if p.Filename != "" {
			out.Attachments = append(out.Attachments, Attachment{Name: p.Filename, Type: p.MimeType, Size: p.Body.Size})
		}
	})
	return out
}

func walk(p part, f func(part)) {
	f(p)
	for _, c := range p.Parts {
		walk(c, f)
	}
}

// text is a message's body as text: its plain part, or else its HTML part
// as text.
func text(p part) string {
	var plain, rich string
	walk(p, func(p part) {
		if p.Filename != "" || p.Body.Data == "" {
			return
		}
		switch {
		case p.MimeType == "text/plain" && plain == "":
			plain = decode(p.Body.Data)
		case p.MimeType == "text/html" && rich == "":
			rich = decode(p.Body.Data)
		}
	})
	if plain != "" {
		return plain
	}
	return htmlText(rich)
}

// decode reads Gmail's URL-safe base64, with or without padding.
func decode(data string) string {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(data, "="))
	if err != nil {
		return ""
	}
	return string(b)
}

var (
	hidden = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)>`)
	breaks = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|tr|h[1-6]|blockquote)>`)
	items  = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	tags   = regexp.MustCompile(`<[^>]*>`)
	spaces = regexp.MustCompile(`[ \t\x{a0}]+`)
)

// htmlText turns an HTML body into text good enough to read: paragraphs,
// line breaks and list items kept, everything else dropped.
func htmlText(s string) string {
	s = hidden.ReplaceAllString(s, "")
	s = breaks.ReplaceAllString(s, "\n")
	s = items.ReplaceAllString(s, "- ")
	s = html.UnescapeString(tags.ReplaceAllString(s, ""))
	return spaces.ReplaceAllString(s, " ")
}

func (a *Adapter) get(ctx context.Context, s workspace.Settings, mid, format string, headers ...string) (message, error) {
	mid, err := id("message", mid)
	if err != nil {
		return message{}, err
	}
	q := url.Values{"format": {format}}
	if len(headers) > 0 {
		q["metadataHeaders"] = headers
	}
	var m message
	if err := a.call(ctx, s, http.MethodGet, "/messages/"+mid, q, nil, &m); err != nil {
		return m, err
	}
	if m.ID == "" {
		return m, fmt.Errorf("there is no message %s", mid)
	}
	return m, nil
}

func (a *Adapter) search(ctx context.Context, s workspace.Settings, in struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
},
) (any, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		query = "in:inbox"
	}
	var list struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
		Estimate int `json:"resultSizeEstimate"`
	}
	err := a.call(ctx, s, http.MethodGet, "/messages", url.Values{
		"q": {query}, "maxResults": {strconv.Itoa(apitool.Limit(in.Limit, 20, 50))},
	}, nil, &list)
	if err != nil {
		return nil, err
	}
	out := []Brief{}
	for _, l := range list.Messages {
		m, err := a.get(ctx, s, l.ID, "metadata", "From", "To", "Subject", "Date")
		if err != nil {
			return nil, err
		}
		out = append(out, m.brief())
	}
	return map[string]any{"messages": out, "estimated_total": list.Estimate}, nil
}

func (a *Adapter) readMessage(ctx context.Context, s workspace.Settings, in struct {
	ID string `json:"id"`
},
) (any, error) {
	m, err := a.get(ctx, s, in.ID, "full")
	if err != nil {
		return nil, err
	}
	return m.full(maxBody), nil
}

func (a *Adapter) readThread(ctx context.Context, s workspace.Settings, in struct {
	ThreadID string `json:"thread_id"`
},
) (any, error) {
	tid, err := id("thread", in.ThreadID)
	if err != nil {
		return nil, err
	}
	var t struct {
		Messages []message `json:"messages"`
	}
	if err := a.call(ctx, s, http.MethodGet, "/threads/"+tid, url.Values{"format": {"full"}}, nil, &t); err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(t.Messages))
	for _, m := range t.Messages {
		out = append(out, m.full(maxThreadBody))
	}
	return out, nil
}

type label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func (a *Adapter) labels(ctx context.Context, s workspace.Settings) ([]label, error) {
	var r struct {
		Labels []label `json:"labels"`
	}
	err := a.call(ctx, s, http.MethodGet, "/labels", nil, nil, &r)
	return r.Labels, err
}

func (a *Adapter) listLabels(ctx context.Context, s workspace.Settings, _ struct{}) (any, error) {
	ls, err := a.labels(ctx, s)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(ls, func(x, y label) int { return strings.Compare(x.Type+x.Name, y.Type+y.Name) })
	return ls, nil
}

// outgoing is a message to send or save as a draft.
type outgoing struct {
	to, cc, bcc []*mail.Address
	subject     string
	body        string
	// inReplyTo, references and threadID put a reply in its thread.
	inReplyTo, references, threadID string
}

// oneLine keeps a header's value to one line, so nothing can add headers.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// raw is the message in RFC 5322, as Gmail's raw field takes it. Gmail adds
// From and Date, and drops Bcc after sending to it.
func (o outgoing) raw() string {
	var b bytes.Buffer
	head := func(name, value string) {
		if value != "" {
			b.WriteString(name + ": " + value + "\r\n")
		}
	}
	list := func(as []*mail.Address) string {
		parts := make([]string, len(as))
		for i, a := range as {
			parts[i] = a.String()
		}
		return strings.Join(parts, ", ")
	}
	head("To", list(o.to))
	head("Cc", list(o.cc))
	head("Bcc", list(o.bcc))
	head("Subject", mime.QEncoding.Encode("utf-8", oneLine(o.subject)))
	head("In-Reply-To", oneLine(o.inReplyTo))
	head("References", oneLine(o.references))
	head("MIME-Version", "1.0")
	head("Content-Type", `text/plain; charset="UTF-8"`)
	head("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(o.body, "\r\n", "\n"), "\n", "\r\n")))
	_ = w.Close()
	return base64.URLEncoding.EncodeToString(b.Bytes())
}

// payload is the message as Gmail's send and drafts take it.
func (o outgoing) payload() map[string]string {
	p := map[string]string{"raw": o.raw()}
	if o.threadID != "" {
		p["threadId"] = o.threadID
	}
	return p
}

// replyTo prepares an answer to message mid: to its sender, or with all,
// to everyone on it but the mailbox, in its thread, above the quoted
// original.
func (a *Adapter) replyTo(ctx context.Context, s workspace.Settings, mid string, all bool, body string) (outgoing, error) {
	m, err := a.get(ctx, s, mid, "full")
	if err != nil {
		return outgoing{}, err
	}
	var self struct {
		Email string `json:"emailAddress"`
	}
	if err := a.call(ctx, s, http.MethodGet, "/profile", nil, nil, &self); err != nil {
		return outgoing{}, err
	}
	h := m.Payload.header
	o := outgoing{threadID: m.ThreadID, inReplyTo: h("Message-ID")}
	o.references = strings.TrimSpace(h("References") + " " + o.inReplyTo)
	o.subject = h("Subject")
	if !strings.HasPrefix(strings.ToLower(o.subject), "re:") {
		o.subject = "Re: " + o.subject
	}
	seen := map[string]bool{strings.ToLower(self.Email): true}
	add := func(into *[]*mail.Address, header string) {
		list, _ := mail.ParseAddressList(header)
		for _, addr := range list {
			if key := strings.ToLower(addr.Address); !seen[key] {
				seen[key] = true
				*into = append(*into, addr)
			}
		}
	}
	add(&o.to, cmp.Or(strings.TrimSpace(h("Reply-To")), h("From")))
	if all {
		add(&o.cc, h("To"))
		add(&o.cc, h("Cc"))
	}
	if len(o.to) == 0 {
		// A message the mailbox sent itself: answer whom it went to.
		add(&o.to, h("To"))
	}
	o.body = body + "\n\nOn " + h("Date") + ", " + h("From") + " wrote:\n" + quote(mailtext.Body(text(m.Payload), maxThreadBody))
	return o, nil
}

// quote marks each line as quoted.
func quote(s string) string {
	return "> " + strings.ReplaceAll(s, "\n", "\n> ")
}

type newMessage struct {
	To      []string `json:"to"`
	Cc      []string `json:"cc"`
	Bcc     []string `json:"bcc"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
	ReplyTo string   `json:"reply_to"`
}

// compose checks a new message's addresses into o.
func (n newMessage) compose(o *outgoing) error {
	for _, f := range []struct {
		name string
		in   []string
		out  *[]*mail.Address
	}{{"to", n.To, &o.to}, {"cc", n.Cc, &o.cc}, {"bcc", n.Bcc, &o.bcc}} {
		if len(f.in) == 0 {
			continue
		}
		list, err := mailtext.Addresses(f.name, f.in)
		if err != nil {
			return err
		}
		*f.out = list
	}
	if strings.TrimSpace(n.Subject) != "" {
		o.subject = n.Subject
	}
	return nil
}

type sent struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

func (a *Adapter) send(ctx context.Context, s workspace.Settings, in newMessage) (any, error) {
	var o outgoing
	if err := in.compose(&o); err != nil {
		return nil, err
	}
	if len(o.to) == 0 {
		return nil, errors.New("give to, at least one address")
	}
	if err := apitool.Need("subject", o.subject); err != nil {
		return nil, err
	}
	o.body = in.Body
	var r sent
	if err := a.call(ctx, s, http.MethodPost, "/messages/send", nil, o.payload(), &r); err != nil {
		return nil, err
	}
	return map[string]any{"sent": true, "id": r.ID, "thread_id": r.ThreadID}, nil
}

func (a *Adapter) reply(ctx context.Context, s workspace.Settings, in struct {
	ID       string `json:"id"`
	Body     string `json:"body"`
	ReplyAll bool   `json:"reply_all"`
},
) (any, error) {
	if err := apitool.Need("body", in.Body); err != nil {
		return nil, err
	}
	o, err := a.replyTo(ctx, s, in.ID, in.ReplyAll, in.Body)
	if err != nil {
		return nil, err
	}
	if len(o.to) == 0 {
		return nil, errors.New("the message has nobody to answer")
	}
	var r sent
	if err := a.call(ctx, s, http.MethodPost, "/messages/send", nil, o.payload(), &r); err != nil {
		return nil, err
	}
	return map[string]any{"sent": true, "id": r.ID, "thread_id": r.ThreadID, "to": names(o.to), "cc": names(o.cc)}, nil
}

func names(as []*mail.Address) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Address
	}
	return out
}

func (a *Adapter) draft(ctx context.Context, s workspace.Settings, in newMessage) (any, error) {
	if err := apitool.Need("body", in.Body); err != nil {
		return nil, err
	}
	var o outgoing
	if strings.TrimSpace(in.ReplyTo) != "" {
		var err error
		if o, err = a.replyTo(ctx, s, in.ReplyTo, false, in.Body); err != nil {
			return nil, err
		}
	} else {
		o.body = in.Body
	}
	if err := in.compose(&o); err != nil {
		return nil, err
	}
	if err := apitool.Need("subject", o.subject); err != nil {
		return nil, err
	}
	var r struct {
		ID      string `json:"id"`
		Message sent   `json:"message"`
	}
	req := map[string]any{"message": o.payload()}
	if err := a.call(ctx, s, http.MethodPost, "/drafts", nil, req, &r); err != nil {
		return nil, err
	}
	return map[string]any{"saved": true, "draft_id": r.ID, "id": r.Message.ID, "thread_id": r.Message.ThreadID}, nil
}

func (a *Adapter) changeLabels(ctx context.Context, s workspace.Settings, in struct {
	ID     string   `json:"id"`
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
},
) (any, error) {
	mid, err := id("message", in.ID)
	if err != nil {
		return nil, err
	}
	if len(in.Add) == 0 && len(in.Remove) == 0 {
		return nil, errors.New("give labels to add or remove")
	}
	ls, err := a.labels(ctx, s)
	if err != nil {
		return nil, err
	}
	resolve := func(names []string) ([]string, error) {
		out := []string{}
		for _, n := range names {
			n = strings.TrimSpace(n)
			i := slices.IndexFunc(ls, func(l label) bool { return l.ID == n || strings.EqualFold(l.Name, n) })
			if i < 0 {
				return nil, fmt.Errorf("there is no label %q; list_labels lists them", n)
			}
			out = append(out, ls[i].ID)
		}
		return out, nil
	}
	add, err := resolve(in.Add)
	if err != nil {
		return nil, err
	}
	remove, err := resolve(in.Remove)
	if err != nil {
		return nil, err
	}
	var m message
	req := map[string][]string{"addLabelIds": add, "removeLabelIds": remove}
	if err := a.call(ctx, s, http.MethodPost, "/messages/"+mid+"/modify", nil, req, &m); err != nil {
		return nil, err
	}
	return map[string]any{"id": m.ID, "labels": m.LabelIDs}, nil
}
