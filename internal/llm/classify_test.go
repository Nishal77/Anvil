package llm

import (
	"errors"
	"fmt"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"
)

// These test the error-classification logic in isolation from any
// network call — CLAUDE.md T3: no test in this repo calls a real LLM
// API, so classifyAnthropicError/classifyGeminiError are exercised
// against synthetic SDK error values instead of live 429/5xx responses.

func TestClassifyAnthropicError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"429 is rate limited", &anthropic.Error{StatusCode: 429}, ErrRateLimited},
		{"500 is unavailable", &anthropic.Error{StatusCode: 500}, ErrProviderUnavailable},
		{"503 is unavailable", &anthropic.Error{StatusCode: 503}, ErrProviderUnavailable},
		{"400 is fatal", &anthropic.Error{StatusCode: 400}, ErrProviderFatal},
		{"401 is fatal", &anthropic.Error{StatusCode: 401}, ErrProviderFatal},
		{"connection error is unavailable", fmt.Errorf("dial tcp: connection refused"), ErrProviderUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyAnthropicError(tt.err)
			if !errors.Is(got, tt.want) {
				t.Fatalf("classifyAnthropicError(%v) = %v, want wrapping %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestClassifyGeminiError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"429 is rate limited", genai.APIError{Code: 429}, ErrRateLimited},
		{"500 is unavailable", genai.APIError{Code: 500}, ErrProviderUnavailable},
		{"503 is unavailable", genai.APIError{Code: 503}, ErrProviderUnavailable},
		{"400 is fatal", genai.APIError{Code: 400}, ErrProviderFatal},
		{"connection error is unavailable", fmt.Errorf("dial tcp: connection refused"), ErrProviderUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyGeminiError(tt.err)
			if !errors.Is(got, tt.want) {
				t.Fatalf("classifyGeminiError(%v) = %v, want wrapping %v", tt.err, got, tt.want)
			}
		})
	}
}
