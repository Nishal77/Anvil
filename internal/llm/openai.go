package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/openai/openai-go/v2"
	"github.com/openai/openai-go/v2/option"
	"github.com/openai/openai-go/v2/shared"
)

const openaiDefaultMaxOutputTokens = 4096

// OpenAIProvider adapts ChatGPT (Chat Completions API) to the
// Provider interface.
type OpenAIProvider struct {
	client openai.Client
	model  string
}

// NewOpenAIProvider constructs an OpenAIProvider for model,
// authenticating with apiKey. The SDK's own retry is disabled
// (WithMaxRetries(0)): Router owns every retry decision, and letting
// the SDK retry too would double the backoff and could retry a
// non-idempotent partial response.
func NewOpenAIProvider(apiKey, model string) *OpenAIProvider {
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(0),
	)
	return &OpenAIProvider{client: client, model: model}
}

// Name identifies this provider for breaker state and metric labels.
func (p *OpenAIProvider) Name() string { return "openai-" + p.model }

// Complete sends req to ChatGPT using native tool-calling — never
// prose-parsed JSON.
func (p *OpenAIProvider) Complete(ctx context.Context, req Request) (Response, error) {
	params := openai.ChatCompletionNewParams{
		Model:               p.model,
		MaxCompletionTokens: openai.Int(int64(maxOutputTokensOrDefault(req.MaxOutputTokens, openaiDefaultMaxOutputTokens))),
	}
	if req.System != "" {
		params.Messages = append(params.Messages, openai.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		params.Messages = append(params.Messages, toOpenAIMessage(m))
	}
	for _, t := range req.Tools {
		params.Tools = append(params.Tools, toOpenAITool(t))
	}

	resp, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return Response{}, classifyOpenAIError(err)
	}
	if len(resp.Choices) == 0 {
		return Response{}, fmt.Errorf("%w: no choices in response", ErrProviderFatal)
	}

	msg := resp.Choices[0].Message
	result := Response{
		Text:  msg.Content,
		Model: resp.Model,
		Usage: Usage{
			InputTokens:       resp.Usage.PromptTokens,
			OutputTokens:      resp.Usage.CompletionTokens,
			CachedInputTokens: resp.Usage.PromptTokensDetails.CachedTokens,
		},
	}
	for _, tc := range msg.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, ToolCall{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	return result, nil
}

func toOpenAIMessage(m Message) openai.ChatCompletionMessageParamUnion {
	switch m.Role {
	case RoleTool:
		return openai.ToolMessage(m.ToolResult, m.ToolCallID)
	case RoleAssistant:
		assistant := openai.ChatCompletionAssistantMessageParam{}
		if m.Content != "" {
			assistant.Content.OfString = openai.String(m.Content)
		}
		for _, tc := range m.ToolCalls {
			assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: tc.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      tc.Name,
						Arguments: string(tc.Input),
					},
				},
			})
		}
		return openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}
	default: // RoleUser and RoleSystem (system content is prepended separately in Complete)
		return openai.UserMessage(m.Content)
	}
}

func toOpenAITool(t Tool) openai.ChatCompletionToolUnionParam {
	var params shared.FunctionParameters
	if len(t.InputSchema) > 0 {
		_ = json.Unmarshal(t.InputSchema, &params) // malformed schema: OpenAI rejects the tool with a 4xx, surfaced as ErrProviderFatal
	}
	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        t.Name,
		Description: openai.String(t.Description),
		Parameters:  params,
	})
}

// classifyOpenAIError maps an SDK error to this package's sentinels.
func classifyOpenAIError(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		// Not apiErr.Error(): it dereferences Request/Response fields
		// the SDK only populates on a real round trip.
		switch {
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return fmt.Errorf("%w: status %d", ErrRateLimited, apiErr.StatusCode)
		case apiErr.StatusCode >= 500:
			return fmt.Errorf("%w: status %d", ErrProviderUnavailable, apiErr.StatusCode)
		default:
			return fmt.Errorf("%w: status %d", ErrProviderFatal, apiErr.StatusCode)
		}
	}
	return fmt.Errorf("%w: %s", ErrProviderUnavailable, err.Error())
}
