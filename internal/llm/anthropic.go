package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const anthropicDefaultMaxOutputTokens = 4096

// AnthropicProvider adapts Claude to the Provider interface. It is the
// fallback in PRD §9.4's provider ladder — Gemini free tier is
// primary, Anthropic covers the failover and private-content path.
type AnthropicProvider struct {
	client anthropic.Client
	model  anthropic.Model
}

// NewAnthropicProvider constructs an AnthropicProvider for model,
// authenticating with apiKey. The SDK's own retry is disabled
// (WithMaxRetries(0)): Router owns every retry decision, and letting
// the SDK retry too would double the backoff and could retry a
// non-idempotent partial stream.
func NewAnthropicProvider(apiKey string, model anthropic.Model) *AnthropicProvider {
	client := anthropic.NewClient(
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(0),
	)
	return &AnthropicProvider{client: client, model: model}
}

// Name identifies this provider for breaker state and metric labels.
func (p *AnthropicProvider) Name() string { return "anthropic-" + string(p.model) }

// Complete sends req to Claude using native tool-calling — never
// prose-parsed JSON — and classifies any error into this package's
// sentinels so Router can branch with errors.Is.
func (p *AnthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	params := anthropic.MessageNewParams{
		Model:     p.model,
		MaxTokens: int64(maxOutputTokensOrDefault(req.MaxOutputTokens, anthropicDefaultMaxOutputTokens)),
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	for _, m := range req.Messages {
		params.Messages = append(params.Messages, toAnthropicMessage(m))
	}
	for _, t := range req.Tools {
		params.Tools = append(params.Tools, anthropic.ToolUnionParamOfTool(toAnthropicInputSchema(t.InputSchema), t.Name))
	}

	msg, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return Response{}, classifyAnthropicError(err)
	}

	resp := Response{
		Usage: Usage{
			InputTokens:       msg.Usage.InputTokens,
			OutputTokens:      msg.Usage.OutputTokens,
			CachedInputTokens: msg.Usage.CacheReadInputTokens,
		},
		Model: string(msg.Model),
	}
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			resp.Text += block.Text
		case "tool_use":
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{ID: block.ID, Name: block.Name, Input: block.Input})
		}
	}
	return resp, nil
}

func toAnthropicMessage(m Message) anthropic.MessageParam {
	switch m.Role {
	case RoleTool:
		return anthropic.NewUserMessage(anthropic.NewToolResultBlock(m.ToolCallID, m.ToolResult, false))
	case RoleAssistant:
		blocks := make([]anthropic.ContentBlockParamUnion, 0, 1+len(m.ToolCalls))
		if m.Content != "" {
			blocks = append(blocks, anthropic.NewTextBlock(m.Content))
		}
		for _, tc := range m.ToolCalls {
			blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, json.RawMessage(tc.Input), tc.Name))
		}
		return anthropic.NewAssistantMessage(blocks...)
	default: // RoleUser and RoleSystem (system content goes in params.System, not here)
		return anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content))
	}
}

// classifyAnthropicError maps an SDK error to this package's
// sentinels: 429 is a normal, expected condition (BUILD-PLAN W5
// non-negotiable), 5xx/timeouts/connection failures are transient,
// and everything else (4xx other than 429) is fatal — retrying it
// against every provider in the ladder would just repeat a bug.
func classifyAnthropicError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		// Not apiErr.Error(): it dereferences Request/Response fields
		// the SDK only populates on a real round trip, and panics on a
		// bare status-only Error value.
		switch {
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return fmt.Errorf("%w: status %d", ErrRateLimited, apiErr.StatusCode)
		case apiErr.StatusCode >= 500:
			return fmt.Errorf("%w: status %d", ErrProviderUnavailable, apiErr.StatusCode)
		default:
			return fmt.Errorf("%w: status %d", ErrProviderFatal, apiErr.StatusCode)
		}
	}
	// No structured status code: connection error, timeout, DNS
	// failure — treat as transient and retryable.
	return fmt.Errorf("%w: %s", ErrProviderUnavailable, err.Error())
}

// toAnthropicInputSchema decodes Tool.InputSchema — a full JSON Schema
// object ({"type":"object","properties":{...},"required":[...]}) —
// into the SDK's split representation. It cannot be assigned wholesale
// to ToolInputSchemaParam.Properties, which is the properties map
// alone, not the enclosing schema object.
func toAnthropicInputSchema(schema json.RawMessage) anthropic.ToolInputSchemaParam {
	var parsed struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if len(schema) > 0 {
		_ = json.Unmarshal(schema, &parsed) // malformed schema: fall through with an empty object, provider rejects with a 4xx we surface as ErrProviderFatal
	}
	return anthropic.ToolInputSchemaParam{Properties: parsed.Properties, Required: parsed.Required}
}

func maxOutputTokensOrDefault(requested, def int) int {
	if requested <= 0 {
		return def
	}
	return requested
}
