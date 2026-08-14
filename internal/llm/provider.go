package llm

import (
	"context"
	"encoding/json"
)

// TaskClass selects which model tier handles a request (FR-031). The
// Router's provider ladder is keyed by this, and only this — never a
// string model name compared scattered through call sites.
type TaskClass string

const (
	TaskPlanning      TaskClass = "PLANNING"
	TaskExecution     TaskClass = "EXECUTION"
	TaskSummarization TaskClass = "SUMMARIZATION"
)

// Role identifies the speaker of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a provider-neutral conversation.
type Message struct {
	Role Role
	// Content is the message text. Empty on an assistant message that
	// only carries ToolCalls.
	Content string
	// ToolCalls is set on an assistant message that invoked one or more
	// tools; ToolCallID is set on the RoleTool message answering one of
	// them.
	ToolCalls  []ToolCall
	ToolCallID string
	ToolResult string
}

// Tool describes one callable function via JSON Schema, passed to the
// provider's native tool-calling. Every adapter must use structured
// output — never regex a JSON block out of a prose response.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolCall is a model-issued invocation of one of Request.Tools,
// decoded from the provider's native structured-output field.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Usage reports tokens as billed by the provider — the source of truth
// the AFTER budget check (FR-033) uses. CachedInputTokens is tracked
// separately so cost math doesn't bill cached input at the full rate
// (NFR-013).
type Usage struct {
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
}

// Request is a provider-neutral completion request.
type Request struct {
	TaskClass       TaskClass
	System          string
	Messages        []Message
	Tools           []Tool
	MaxOutputTokens int
}

// Response is a provider-neutral completion result.
type Response struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
	// Model is the provider's own model identifier, used for cost
	// lookup and metric labels.
	Model string
}

// Provider is one LLM backend. Implementations live one per file
// (gemini.go, anthropic.go, fake.go). A Provider must never retry
// internally — retry and failover are the Router's job (FR-032,
// FR-035); a self-retrying Provider would double the backoff.
type Provider interface {
	// Complete sends one request and returns one response. A non-nil
	// error is always one of the sentinels in errors.go, so Router can
	// branch with errors.Is — never raw status-code or string matching
	// outside this package.
	Complete(ctx context.Context, req Request) (Response, error)

	// Name identifies the provider for metric labels and circuit
	// breaker state keys ("gemini", "anthropic-haiku").
	Name() string
}
