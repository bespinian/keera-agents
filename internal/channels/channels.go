// Package channels connects agents to the places a business works in: chat
// services (Microsoft Teams, Google Chat, Slack), file stores (Google Drive,
// OneDrive and SharePoint), code hosts (GitHub, GitLab), and the business's
// other services, which agents call through typed tools (Jira, Salesforce,
// AWS and more).
//
// A Connection is one organisation's credentials for one service, such as
// its Slack app. Each kind has an adapter in a package of its own, which
// registers itself here. The control plane only ever talks to Chat, Files,
// Code and API.
package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

	Jira           Kind = "jira"
	Confluence     Kind = "confluence"
	GoogleDocs     Kind = "gdocs"
	GoogleSheets   Kind = "gsheets"
	GoogleSlides   Kind = "gslides"
	GoogleCalendar Kind = "gcalendar"
	Gmail          Kind = "gmail"
	Outlook        Kind = "outlook"
	HubSpot        Kind = "hubspot"
	Salesforce     Kind = "salesforce"
	Postgres       Kind = "postgres"
	Kubernetes     Kind = "kubernetes"
	AWS            Kind = "aws"
	Azure          Kind = "azure"
	GCP            Kind = "gcp"
)

// AnyTarget is a binding to everything a connection reaches: every
// conversation a chat bot is in, or every project an API connector sees.
const AnyTarget = "*"

// Connection is one organisation's credentials for one service. Secrets are
// decrypted only to make a call, and never sent to the panel or an agent.
type Connection struct {
	ID      string
	Kind    Kind
	Config  map[string]string
	Secrets map[string]string
	// PerUser is a connection each person signs in to with their own
	// account. Its secrets are then only the OAuth client's. A call made for
	// someone carries their access Token, and Account, who they are at the
	// service; without one, as when the connector is checked, it has
	// neither.
	PerUser bool
	Token   string
	Account string
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
	// Vendor makes the service, such as Google. Category groups kinds in the
	// panel's catalogue, such as Chat or Cloud. Summary says in one sentence
	// what agents do with it.
	Vendor   string `json:"vendor"`
	Category string `json:"category"`
	Summary  string `json:"summary"`
	// Chat kinds take messages and send replies. Files kinds are watched
	// folders and file tools. Code kinds hold repositories agents clone,
	// push to and open changes in. API kinds give agents Tools. Register
	// sets all four, and Tools, from the interfaces the adapter implements.
	Chat   bool    `json:"chat"`
	Files  bool    `json:"files"`
	Code   bool    `json:"code"`
	API    bool    `json:"api"`
	Tools  []Tool  `json:"tools,omitempty"`
	Fields []Field `json:"fields"`
	// Setup lists, as numbered Markdown steps, what to create on the
	// service's side. It may name {webhook}, which the panel replaces with
	// the address to register.
	Setup string `json:"setup"`
	// TargetLabel and TargetHelp describe what a binding names: a channel,
	// a space, a folder, a Jira project. An API kind without a TargetLabel
	// has nothing to narrow to, and its bindings are AnyTarget.
	TargetLabel string `json:"target_label,omitempty"`
	TargetHelp  string `json:"target_help,omitempty"`
	// AnyTarget is an API kind whose binding may be AnyTarget: everything
	// the connection reaches.
	AnyTarget bool `json:"any_target,omitempty"`
	// SignIn is set for a kind whose connectors may let each person sign
	// in with their own account. Register sets it from the PerUser
	// interface.
	SignIn *SignIn `json:"sign_in,omitempty"`
}

// SignIn describes the OAuth client an administrator registers so that
// people can sign in to a service themselves.
type SignIn struct {
	// Fields replace the Descriptor's for a connector people sign in to.
	Fields []Field `json:"fields"`
	// Setup is like the Descriptor's. It may name {callback}, which the
	// panel replaces with the address to register.
	Setup string `json:"setup"`
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

// Tool is one thing an API kind lets agents do. Agents see it as
// <kind>_<name>, with a "connector" argument added for the binding's id.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Parameters is the JSON Schema of the arguments, an object.
	Parameters json.RawMessage `json:"-"`
	// Write changes something in the service. A read-only binding has no
	// such tools.
	Write bool `json:"write,omitempty"`
}

// API is a service agents call through typed tools, such as Jira or AWS.
type API interface {
	Adapter
	Tools() []Tool
	// Target checks a binding's target before it is saved, with the
	// connection, and returns it as it should be stored, such as a Jira
	// project's key in capitals. AnyTarget comes only for a kind that
	// allows it.
	Target(ctx context.Context, conn Connection, target string) (string, error)
	// Call runs one tool. The adapter keeps it within target, unless that
	// is AnyTarget. args are the tool's own arguments; the result is
	// returned to the model as JSON, or as it is if it is a string.
	Call(ctx context.Context, conn Connection, target, tool string, args json.RawMessage) (any, error)
}

// Grant is what signing someone in gives: a refresh token to get access
// tokens with, the first access token, and who they are at the service.
type Grant struct {
	Refresh string
	Access  string
	Expiry  time.Time
	Account string
}

// PerUser is an adapter whose connectors may let each person sign in with
// their own account, through OAuth with an authorization code and PKCE.
// Each takes a connection with PerUser set and the client in its secrets.
type PerUser interface {
	Adapter
	SignIn() SignIn
	// SignInURL is where the person's browser goes to sign in. redirect is
	// where the service sends them back with a code.
	SignInURL(conn Connection, redirect, state, challenge string) (string, error)
	// Exchange redeems the code they came back with.
	Exchange(ctx context.Context, conn Connection, redirect, code, verifier string) (Grant, error)
	// Refresh gets a new access token. Its Refresh is set only when the
	// service replaced the refresh token.
	Refresh(ctx context.Context, conn Connection, refresh string) (Grant, error)
}

// Options are what every adapter is built with, so tests can point it at a
// fake service.
type Options struct {
	HTTP *http.Client
	// Dial connects as HTTP does, for a service that is not HTTP, such as
	// a database. Nil dials directly, as tests need.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Endpoints overrides a service's base URLs by name, as each adapter
	// documents. Empty uses the real service.
	Endpoints map[string]string
}

// order is how the panel lists the kinds: chats first, then files, code
// hosts and the rest.
var order = []Kind{
	Teams, GoogleChat, Slack, GoogleDrive, OneDrive, GitHub, GitLab,
	Jira, Confluence, GoogleDocs, GoogleSheets, GoogleSlides, GoogleCalendar, Gmail, Outlook,
	HubSpot, Salesforce, Postgres, Kubernetes, AWS, Azure, GCP,
}

var (
	mu       sync.RWMutex
	descs    = map[Kind]Descriptor{}
	adapters = map[Kind]Adapter{}
	chats    = map[Kind]Chat{}
	files    = map[Kind]Files{}
	codes    = map[Kind]Code{}
	apis     = map[Kind]API{}
	perUsers = map[Kind]PerUser{}
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
	if x, ok := a.(API); ok {
		apis[d.Kind], d.API, d.Tools = x, true, x.Tools()
	}
	if p, ok := a.(PerUser); ok {
		signIn := p.SignIn()
		perUsers[d.Kind], d.SignIn = p, &signIn
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

func APIFor(k Kind) (API, bool) {
	mu.RLock()
	defer mu.RUnlock()
	x, ok := apis[k]
	return x, ok
}

// PerUserFor returns the adapter of a kind people may sign in to.
func PerUserFor(k Kind) (PerUser, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := perUsers[k]
	return p, ok
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
