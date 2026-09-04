// Package provider defines the model-facing interface and its wire types.
// Only an OpenAI-compatible implementation ships today; the interface is the
// seam an Anthropic or Bedrock adapter would slot into.
package provider

import "context"

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry in a conversation.
type Message struct {
	Role Role   `json:"role"`
	Text string `json:"content"`
	// Internal marks context written by the runtime rather than the user.
	// It remains in provider requests, but transcript renderers must not
	// attribute it to the person at the terminal.
	Internal bool `json:"internal,omitempty"`

	// ToolCalls is set on assistant messages that requested tool execution.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a RoleTool message back to the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Name is the tool name for RoleTool messages.
	Name string `json:"name,omitempty"`
}

// ToolCall is a model request to invoke a tool.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Args is the raw JSON argument object, kept unparsed so a malformed
	// payload can be reported back to the model verbatim.
	Args string `json:"arguments"`
}

// ToolDef is a tool advertised to the model. Whether a tool appears in a
// request at all is decided by the enforcer, which is what makes masking
// structural rather than advisory.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolChoiceMode selects how the provider constrains tool use.
type ToolChoiceMode string

const (
	// ToolChoiceAuto lets the model decide.
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceNone forbids tool calls.
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceRequired demands some tool call.
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceFunction pins the next call to one named tool. This is the
	// lever the plugin could not reach, and the main reason weak models can
	// be made to delegate reliably.
	ToolChoiceFunction ToolChoiceMode = "function"
)

// ToolChoice constrains the model's next action.
type ToolChoice struct {
	Mode ToolChoiceMode
	// Name is required when Mode is ToolChoiceFunction.
	Name string
}

// Request is one turn's input.
type Request struct {
	Model       string
	Messages    []Message
	Tools       []ToolDef
	ToolChoice  *ToolChoice
	Temperature *float64
	MaxTokens   *int

	// Extra is merged into the request body, for endpoint-specific knobs such
	// as llama.cpp grammars or vLLM guided decoding.
	Extra map[string]any
}

// Usage reports token accounting when the endpoint supplies it.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// EventKind discriminates a stream event.
type EventKind string

const (
	// EventText carries an incremental assistant text delta.
	EventText EventKind = "text"
	// EventToolCall carries a fully assembled tool call.
	EventToolCall EventKind = "tool_call"
	// EventDone is the final event, carrying usage and stop reason.
	EventDone EventKind = "done"
)

// Event is one item from a streaming response.
type Event struct {
	Kind       EventKind
	Text       string
	ToolCall   *ToolCall
	Usage      *Usage
	StopReason string
}

// Stream yields events until it returns io.EOF from Recv.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// Provider is a model endpoint.
type Provider interface {
	// Name identifies the provider for logging and the status bar.
	Name() string
	// Stream issues a request and returns its event stream.
	Stream(ctx context.Context, req Request) (Stream, error)
}
