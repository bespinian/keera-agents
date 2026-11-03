// Package channels connects agents to the places a business works in: chat
// services (Microsoft Teams, Google Chat, Slack), file stores (Google Drive,
// OneDrive and SharePoint) and code hosts (GitHub, GitLab).
//
// A Connection is one organisation's credentials for one service, such as
// its Slack app. Each kind has an adapter in a package of its own, which
// registers itself here. The control plane only ever talks to Chat, Files
// and Code.
package channels

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type Kind string

const (
	Slack       Kind = "slack"
	Teams       Kind = "teams"
	GoogleChat  Kind = "googlechat"
	GoogleDrive Kind = "gdrive"
	OneDrive    Kind = "onedrive"
	GitHub      Kind = "github"
	GitLab      Kind = "gitlab"
)

// Connection is one organisation's credentials for one service. Secrets are
// decrypted only to make a call, and never sent to the panel or an agent.
type Connection struct {
	ID      string
	Kind    Kind
	Config  map[string]string
	Secrets map[string]string
}

// Field is one setting of a kind, for the panel's form and for checking a
// connection before it is saved.
type Field struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Help     string `json:"help,omitempty"`
	Secret   bool   `json:"secret,omitempty"`
	Required bool   `json:"required,omitempty"`
	// Multiline is for keys and JSON files pasted whole.
	Multiline bool `json:"multiline,omitempty"`
	// Endpoint is a setting that says where the secrets are sent. Changing
	// it needs the secrets entered again, so whoever edits a connector
	// cannot send its saved token to a server of their own.
	Endpoint bool `json:"endpoint,omitempty"`
}

// Descriptor says what a kind is and what it needs.
type Descriptor struct {
	Kind  Kind   `json:"kind"`
	Label string `json:"label"`
	// Chat kinds take messages and send replies. Files kinds are watched
	// folders and file tools. Code kinds hold repositories agents clone,
	// push to and open changes in. Register sets all three from the
	// interfaces the adapter implements.
	Chat   bool    `json:"chat"`
	Files  bool    `json:"files"`
	Code   bool    `json:"code"`
	Fields []Field `json:"fields"`
	// Setup lists, as numbered Markdown steps, what to create on the
	// service's side. It may name {webhook}, which the panel replaces with
	// the address to register.
	Setup string `json:"setup"`
	// TargetLabel and TargetHelp describe what a binding names: a channel,
	// a space, a folder.
	TargetLabel string `json:"target_label"`
	TargetHelp  string `json:"target_help,omitempty"`
}

// Person is who sent a message. Email is what maps them to a Keera user, so
// an adapter that cannot find it leaves it empty and the message is refused.
type Person struct {
	Email string
	// Outside is someone from another workspace or tenant, such as a guest
	// in a shared channel. Their email is not trusted, so they are refused.
	Outside bool
}

// Target is where a reply goes.
type Target struct {
	// Conversation is the channel, space or chat. It is what a binding names.
	Conversation string `json:"conversation"`
	// Thread is the thread in it, if any.
	Thread string `json:"thread,omitempty"`
	// Extra is whatever else the service needs to answer, such as Teams'
	// service URL.
	Extra map[string]string `json:"extra,omitempty"`
}

// Inbound is one message for an agent.
type Inbound struct {
	Target Target
	Sender Person
	Text   string
	// MessageID makes a redelivered webhook a no-op.
	MessageID string
	// Direct is a one-to-one chat with the bot. Otherwise the message is in
	// a channel, and the bot was mentioned.
	Direct bool
	// ConversationLabel is the channel's name, for the task's context. A
	// direct message needs none.
	ConversationLabel string
}

// Response is what a webhook answers the service, such as Slack's URL
// verification challenge. A zero Response is 200 with no body.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
}

// Receipt is a verified webhook.
type Receipt struct {
	Messages []Inbound
	Response Response
}

// ErrUnverified is a webhook whose signature or token did not check out. It
// is answered 401 and never processed.
var ErrUnverified = errors.New("channels: the request is not from the service")

// Adapter is what every kind's adapter does.
type Adapter interface {
	Descriptor() Descriptor
	// Check verifies a connection's settings with the service before it is
	// saved, such as by asking who the bot is.
	Check(ctx context.Context, conn Connection) error
}

// Chat is a chat service.
type Chat interface {
	Adapter
	// Receive verifies and parses one webhook. body is the whole request
	// body, already read, since verification needs the raw bytes.
	Receive(ctx context.Context, conn Connection, r *http.Request, body []byte) (Receipt, error)
	// Send posts text, in Markdown, and returns the new message's id.
	Send(ctx context.Context, conn Connection, to Target, text string) (string, error)
}

// File is one file or folder in a file store.
type File struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	MimeType string    `json:"mime_type,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Modified time.Time `json:"modified"`
	WebURL   string    `json:"web_url,omitempty"`
	Folder   bool      `json:"folder,omitempty"`
}

// MaxFileBytes bounds what Read returns and Write accepts.
const MaxFileBytes = 4 << 20

// Files is a file store.
type Files interface {
	Adapter
	// List lists a folder by its id. An empty id is the root the connection
	// can see.
	List(ctx context.Context, conn Connection, folder string) ([]File, error)
	// Read returns a file's content as text where the service can convert
	// it, such as a Google Doc, and as stored otherwise, up to MaxFileBytes.
	Read(ctx context.Context, conn Connection, id string) (File, []byte, error)
	// Write creates or replaces the file called name in folder. The caller
	// has checked that name is not empty and has no slashes, that content
	// fits in MaxFileBytes, and has set mimeType.
	Write(ctx context.Context, conn Connection, folder, name, mimeType string, content []byte) (File, error)
	// Changes returns the files added or changed in folder since cursor, and
	// the cursor to ask with next. An empty cursor returns no files, only a
	// cursor to start from, so turning a watch on does not replay the past.
	// A file changed twice may be listed twice, oldest first.
	Changes(ctx context.Context, conn Connection, folder, cursor string) ([]File, string, error)
}

// Repo is a repository on a code host.
type Repo struct {
	// Path is owner/name on GitHub, group/project on GitLab.
	Path          string `json:"path"`
	DefaultBranch string `json:"default_branch"`
}

// Issue is an issue, or a pull or merge request when Change is set.
type Issue struct {
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	Body     string    `json:"body,omitempty"`
	State    string    `json:"state"`
	Author   string    `json:"author,omitempty"`
	Labels   []string  `json:"labels,omitempty"`
	WebURL   string    `json:"web_url,omitempty"`
	Change   bool      `json:"change,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	Updated  time.Time `json:"updated"`
	Comments []Comment `json:"comments,omitempty"`
}

// Brief is the issue without its body, for lists and for a change the
// agent has just written, so the model's context is not spent on it.
func (i Issue) Brief() Issue {
	i.Body = ""
	return i
}

type Comment struct {
	Author string    `json:"author,omitempty"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`
}

// Change asks for a pull request on GitHub, a merge request on GitLab.
type Change struct {
	Branch string
	Base   string
	Title  string
	Body   string
	Draft  bool
}

// Remote is how git reaches a repository: its address, and the user and
// password to send with every request.
type Remote struct {
	URL      string
	User     string
	Password string
}

// Code is a code host.
type Code interface {
	Adapter
	// Repo looks a repository up by its path, to check the connection can
	// reach it and to learn its default branch.
	Repo(ctx context.Context, conn Connection, path string) (Repo, error)
	// Remote returns how git reaches the repository. Its password is the
	// connection's, so it never leaves the control plane.
	Remote(ctx context.Context, conn Connection, path string) (Remote, error)
	// Issues lists issues, or pull or merge requests when changes is set,
	// newest change first. state is open, closed or all.
	Issues(ctx context.Context, conn Connection, path, state string, changes bool) ([]Issue, error)
	// Issue reads one issue, or pull or merge request, with its comments.
	Issue(ctx context.Context, conn Connection, path string, number int, change bool) (Issue, error)
	Comment(ctx context.Context, conn Connection, path string, number int, change bool, body string) error
	// OpenChange opens a pull or merge request.
	OpenChange(ctx context.Context, conn Connection, path string, c Change) (Issue, error)
}

// Options are what every adapter is built with, so tests can point it at a
// fake service.
type Options struct {
	HTTP *http.Client
	// Endpoints overrides a service's base URLs by name, as each adapter
	// documents. Empty uses the real service.
	Endpoints map[string]string
}

// order is how the panel lists the kinds: chats first, code hosts last.
var order = []Kind{Teams, GoogleChat, Slack, GoogleDrive, OneDrive, GitHub, GitLab}

var (
	mu       sync.RWMutex
	descs    = map[Kind]Descriptor{}
	adapters = map[Kind]Adapter{}
	chats    = map[Kind]Chat{}
	files    = map[Kind]Files{}
	codes    = map[Kind]Code{}
)

// Register makes an adapter available. Adapters are created by the server at
// start-up, with its HTTP client.
func Register(a Adapter) {
	d := a.Descriptor()
	mu.Lock()
	defer mu.Unlock()
	adapters[d.Kind] = a
	if c, ok := a.(Chat); ok {
		chats[d.Kind], d.Chat = c, true
	}
	if f, ok := a.(Files); ok {
		files[d.Kind], d.Files = f, true
	}
	if c, ok := a.(Code); ok {
		codes[d.Kind], d.Code = c, true
	}
	descs[d.Kind] = d
}

// AdapterFor returns a kind's adapter, whatever it does.
func AdapterFor(k Kind) (Adapter, bool) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := adapters[k]
	return a, ok
}

func ChatFor(k Kind) (Chat, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := chats[k]
	return c, ok
}

func FilesFor(k Kind) (Files, bool) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := files[k]
	return f, ok
}

func CodeFor(k Kind) (Code, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := codes[k]
	return c, ok
}

// Descriptors lists the registered kinds in order.
func Descriptors() []Descriptor {
	mu.RLock()
	defer mu.RUnlock()
	var out []Descriptor
	for _, k := range order {
		if d, ok := descs[k]; ok {
			out = append(out, d)
		}
	}
	return out
}

// Describe returns one kind's descriptor.
func Describe(k Kind) (Descriptor, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := descs[k]
	return d, ok
}

// SameServer is a CheckRedirect that follows redirects only on the server
// first asked. A code host's address is the organisation's own setting, so
// its server could otherwise send the control plane to an internal address.
// Go keeps custom headers such as GitLab's PRIVATE-TOKEN across hosts, too.
func SameServer(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if first := via[0].URL; req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		return fmt.Errorf("%s redirected to another server, %s; it was not followed", first.Host, req.URL.Host)
	}
	return nil
}
