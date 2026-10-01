// Package protocol is the runtime API between the control plane and the
// agent runtime in each container (`keera-agents agent`).
//
// The runtime calls the control plane, never the other way round, so a
// container needs no open port. It authenticates every call with its runtime
// token as a bearer token:
//
//	GET  /runtime/v1/bootstrap                who it is, its gateway key, its tools
//	GET  /runtime/v1/tasks/next?wait=30s      the next task, or 204 after the wait
//	POST /runtime/v1/tasks/{id}/events        what it did, as it goes
//	POST /runtime/v1/tasks/{id}/finish        how the task ended
//	POST /runtime/v1/tasks/{id}/tools/{name}  a platform tool, run by the control plane
//	*    /runtime/v1/git/{binding}/...        git over HTTP to a repository it is bound to
//
// Platform tools run in the control plane because they need credentials the
// container must never hold: a chat service's bot token, a Drive service
// account, a GitHub token. The container only holds its runtime token and its
// gateway key. Git goes the same way: the runtime serves GitProxyURL inside
// the container, adds its token, and the control plane adds the host's.
package protocol

import (
	"encoding/json"
	"time"
)

const (
	Prefix        = "/runtime/v1"
	BootstrapPath = Prefix + "/bootstrap"
	NextTaskPath  = Prefix + "/tasks/next"
	// TaskPath is followed by /{id}/events, /{id}/finish or
	// /{id}/tools/{name}. A tool call's body is the tool's arguments.
	TaskPath = Prefix + "/tasks"
	// GitPath is followed by /{binding}/ and git's own path, such as
	// /{binding}/web.git/info/refs.
	GitPath = Prefix + "/git"

	// MaxWait bounds a long poll for the next task.
	MaxWait = 50 * time.Second
)

// The environment the control plane gives an agent container.
const (
	EnvRuntimeURL   = "KEERA_AGENTS_RUNTIME_URL"
	EnvRuntimeToken = "KEERA_AGENTS_RUNTIME_TOKEN"
	EnvAgentID      = "KEERA_AGENT_ID"
	// EnvWorkspace is the persistent volume: memory, conversations, files.
	EnvWorkspace = "KEERA_AGENT_WORKSPACE"
)

// GitProxyURL is where git in an agent's commands reaches its repositories:
// the runtime listens there, on the loopback only.
const GitProxyURL = "http://127.0.0.1:7417"

// BranchPrefix starts every branch an agent may push. Others, such as main,
// are refused, so its work always reaches a person as a pull request.
const BranchPrefix = "keera/"

// Workspace is where the volume is mounted when EnvWorkspace is unset.
const Workspace = "/workspace"

// Bootstrap is everything the runtime needs to start. It is fetched again
// when the gateway answers 401, since the key may have been replaced.
type Bootstrap struct {
	// Version changes whenever anything in the bootstrap may have changed:
	// the agent's settings, its key, its tools. Each Task carries the current
	// one, so the runtime fetches the bootstrap again when they differ.
	Version string      `json:"version"`
	Agent   Agent       `json:"agent"`
	Gateway GatewayInfo `json:"gateway"`
	// Tools are the platform tools this agent may call during a task.
	Tools []ToolSpec `json:"tools"`
}

type Agent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Instructions are the agent's own system prompt. The gateway adds the
	// organisation's and project's in front of it.
	Instructions string `json:"instructions"`
	// Model is a gateway alias: a model or a router.
	Model string `json:"model"`
	// Builtins are the runtime's own tools this agent may use: shell, files,
	// memory, web.
	Builtins []string `json:"builtins"`
	// MaxSteps bounds the model calls of one task. Zero means
	// DefaultMaxSteps.
	MaxSteps int    `json:"max_steps"`
	Owner    Person `json:"owner"`
}

// The runtime's builtin tools.
const (
	BuiltinShell  = "shell"
	BuiltinFiles  = "files"
	BuiltinMemory = "memory"
	BuiltinWeb    = "web"
)

// Builtins lists every builtin, in the order the panel shows them.
var Builtins = []string{BuiltinMemory, BuiltinFiles, BuiltinShell, BuiltinWeb}

type GatewayInfo struct {
	// BaseURL is the inference API, such as http://gateway:8080/api/v1.
	BaseURL string `json:"base_url"`
	// APIKey is the agent's own key. The gateway applies its guardrails,
	// budgets and filters to every request made with it.
	APIKey string `json:"api_key"`
	// MCP are the gateway's MCP servers, as URLs that take the same key.
	MCP []MCPServer `json:"mcp,omitempty"`
}

type MCPServer struct {
	Alias string `json:"alias"`
	URL   string `json:"url"`
}

type Person struct {
	Email string `json:"email,omitempty"`
}

// Task sources.
const (
	SourceChat     = "chat"
	SourceSchedule = "schedule"
	SourceBoard    = "board"
	SourceFile     = "file"
	SourceManual   = "manual"
)

// Task is one piece of work for the agent.
type Task struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Prompt string `json:"prompt"`
	// Conversation names the thread a chat task belongs to, so the runtime
	// can keep its history. Empty for a task that stands alone.
	Conversation string `json:"conversation,omitempty"`
	RequestedBy  Person `json:"requested_by"`
	// Context says where the task came from in words the model can use,
	// such as the channel, the card or the file.
	Context  string    `json:"context,omitempty"`
	Deadline time.Time `json:"deadline"`
	// Version is the bootstrap's current Version.
	Version string `json:"version"`
}

// Event kinds.
const (
	EventMessage    = "message"
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventLog        = "log"
)

// MaxEventText bounds an event's text and data; the runner cuts longer text
// and drops longer data.
const MaxEventText = 16 << 10

// Event is one thing the agent did. Text is what the panel shows.
type Event struct {
	Kind string          `json:"kind"`
	Text string          `json:"text"`
	Data json.RawMessage `json:"data,omitempty"`
	At   time.Time       `json:"at"`
}

type Events struct {
	Events []Event `json:"events"`
}

// Finish states.
const (
	Succeeded = "succeeded"
	Failed    = "failed"
)

// Finish ends a task. Result is what goes back to whoever asked: the chat
// thread, the card or the schedule's channel.
type Finish struct {
	State  string `json:"state"`
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// ToolSpec describes a tool in the shape of an OpenAI function.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolResult is what the model sees.
type ToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// DefaultMaxSteps bounds an agent's model calls per task when nobody chose.
// The schema's default is the same.
const DefaultMaxSteps = 30
