package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/genai"
)

const geminiDefaultMaxOutputTokens = 4096

// GeminiProvider adapts Gemini to the Provider interface. It is the
// primary provider in PRD §9.4's ladder: free tier, so 429 (the 10 RPM
// ceiling) is expected, routine operation, not a failure.
type GeminiProvider struct {
	client *genai.Client
	model  string
}

// NewGeminiProvider constructs a GeminiProvider for model,
// authenticating with apiKey.
func NewGeminiProvider(ctx context.Context, apiKey, model string) (*GeminiProvider, error) {
	// Disable google.golang.org/genai's built-in retry (default: 5
	// attempts). Router owns every retry decision — a self-retrying
	// SDK would double the backoff and could retry a non-idempotent
	// partial response.
	noRetry := int32(1)
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey: apiKey,
		HTTPOptions: genai.HTTPOptions{
			RetryOptions: &genai.HTTPRetryOptions{Attempts: &noRetry},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("llm: new gemini provider: %w", err)
	}
	return &GeminiProvider{client: client, model: model}, nil
}

// Name identifies this provider for breaker state and metric labels.
func (p *GeminiProvider) Name() string { return "gemini-" + p.model }

// Complete sends req to Gemini using native function-calling — never
// prose-parsed JSON.
func (p *GeminiProvider) Complete(ctx context.Context, req Request) (Response, error) {
	contents := make([]*genai.Content, 0, len(req.Messages))
	for _, m := range req.Messages {
		contents = append(contents, toGeminiContent(m))
	}

	resp, err := p.client.Models.GenerateContent(ctx, p.model, contents, buildGeminiConfig(req))
	if err != nil {
		return Response{}, classifyGeminiError(err)
	}

	toolCalls, err := extractGeminiToolCalls(resp)
	if err != nil {
		return Response{}, err
	}
	result := Response{
		Text:      resp.Text(),
		Model:     p.model,
		ToolCalls: toolCalls,
	}
	if resp.UsageMetadata != nil {
		result.Usage = Usage{
			InputTokens:       int64(resp.UsageMetadata.PromptTokenCount),
			OutputTokens:      int64(resp.UsageMetadata.CandidatesTokenCount),
			CachedInputTokens: int64(resp.UsageMetadata.CachedContentTokenCount),
		}
	}
	return result, nil
}

// buildGeminiConfig translates the provider-neutral Request into the
// SDK's GenerateContentConfig.
func buildGeminiConfig(req Request) *genai.GenerateContentConfig {
	config := &genai.GenerateContentConfig{
		MaxOutputTokens: int32(maxOutputTokensOrDefault(req.MaxOutputTokens, geminiDefaultMaxOutputTokens)),
	}
	if req.System != "" {
		config.SystemInstruction = genai.NewContentFromText(req.System, genai.RoleUser)
	}
	if len(req.Tools) > 0 {
		config.Tools = []*genai.Tool{{FunctionDeclarations: toGeminiFunctionDeclarations(req.Tools)}}
	}
	return config
}

// extractGeminiToolCalls collects every FunctionCall part across every
// candidate into this package's provider-neutral ToolCall shape.
func extractGeminiToolCalls(resp *genai.GenerateContentResponse) ([]ToolCall, error) {
	var calls []ToolCall
	for _, c := range resp.Candidates {
		if c.Content == nil {
			continue
		}
		for _, part := range c.Content.Parts {
			if part.FunctionCall == nil {
				continue
			}
			input, err := json.Marshal(part.FunctionCall.Args)
			if err != nil {
				return nil, fmt.Errorf("%w: marshal function call args: %s", ErrProviderFatal, err.Error())
			}
			calls = append(calls, ToolCall{ID: part.FunctionCall.ID, Name: part.FunctionCall.Name, Input: input})
		}
	}
	return calls, nil
}

func toGeminiContent(m Message) *genai.Content {
	switch m.Role {
	case RoleTool:
		var response map[string]any
		_ = json.Unmarshal([]byte(m.ToolResult), &response) // non-JSON tool result: falls through as an empty response map, surfaced to the model as no output rather than a harness crash
		return &genai.Content{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: m.ToolCallID, Response: response}}},
		}
	case RoleAssistant:
		parts := make([]*genai.Part, 0, 1+len(m.ToolCalls))
		if m.Content != "" {
			parts = append(parts, genai.NewPartFromText(m.Content))
		}
		for _, tc := range m.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal(tc.Input, &args)
			parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: tc.ID, Name: tc.Name, Args: args}})
		}
		return &genai.Content{Role: genai.RoleModel, Parts: parts}
	default: // RoleUser
		return genai.NewContentFromText(m.Content, genai.RoleUser)
	}
}

func toGeminiFunctionDeclarations(tools []Tool) []*genai.FunctionDeclaration {
	decls := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		var schema genai.Schema
		if len(t.InputSchema) > 0 {
			_ = json.Unmarshal(t.InputSchema, &schema) // malformed schema: Gemini rejects the declaration with a 4xx, surfaced as ErrProviderFatal
		}
		decls = append(decls, &genai.FunctionDeclaration{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  &schema,
		})
	}
	return decls
}

// classifyGeminiError maps an SDK error to this package's sentinels.
// 429 is Gemini's free-tier RPM ceiling — expected, routine operation
// (BUILD-PLAN W5 non-negotiable), not a circuit-breaker failure.
func classifyGeminiError(err error) error {
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Code == http.StatusTooManyRequests:
			return fmt.Errorf("%w: %s", ErrRateLimited, apiErr.Error())
		case apiErr.Code >= 500:
			return fmt.Errorf("%w: %s", ErrProviderUnavailable, apiErr.Error())
		default:
			return fmt.Errorf("%w: %s", ErrProviderFatal, apiErr.Error())
		}
	}
	return fmt.Errorf("%w: %s", ErrProviderUnavailable, err.Error())
}
