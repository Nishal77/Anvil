package llm

import (
	"context"
	"net/http"
	"testing"

	"google.golang.org/genai"
	"gopkg.in/dnaeon/go-vcr.v3/cassette"
	"gopkg.in/dnaeon/go-vcr.v3/recorder"
)

// TestGeminiProvider_Complete_ParsesRecordedResponse is a contract
// test (CLAUDE.md §20.1): real SDK request-building and
// response-parsing against a recorded fixture, never a live API call
// (CLAUDE.md T3).
func TestGeminiProvider_Complete_ParsesRecordedResponse(t *testing.T) {
	rec, err := recorder.NewWithOptions(&recorder.Options{
		CassetteName: "testdata/cassettes/gemini_complete",
		Mode:         recorder.ModeReplayOnly,
	})
	if err != nil {
		t.Fatalf("recorder.NewWithOptions() error = %v", err)
	}
	t.Cleanup(func() {
		if err := rec.Stop(); err != nil {
			t.Errorf("recorder.Stop() error = %v", err)
		}
	})
	rec.SetMatcher(func(r *http.Request, i cassette.Request) bool {
		return r.Method == i.Method
	})

	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:     "test-key-not-real",
		HTTPClient: rec.GetDefaultClient(),
	})
	if err != nil {
		t.Fatalf("genai.NewClient() error = %v", err)
	}
	p := &GeminiProvider{client: client, model: "gemini-2.5-flash"}

	resp, err := p.Complete(context.Background(), Request{
		System:   "you are a test fixture",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Text != "hello from gemini" {
		t.Errorf("Text = %q, want %q", resp.Text, "hello from gemini")
	}
	if resp.Usage.InputTokens != 8 || resp.Usage.OutputTokens != 4 {
		t.Errorf("Usage = %+v, want {InputTokens:8 OutputTokens:4 ...}", resp.Usage)
	}
	if resp.Model != "gemini-2.5-flash" {
		t.Errorf("Model = %q, want gemini-2.5-flash", resp.Model)
	}
}
