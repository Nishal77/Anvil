package llm

import (
	"context"
	"net/http"
	"testing"

	"github.com/openai/openai-go/v2"
	"github.com/openai/openai-go/v2/option"
	"gopkg.in/dnaeon/go-vcr.v3/cassette"
	"gopkg.in/dnaeon/go-vcr.v3/recorder"
)

// TestOpenAIProvider_Complete_ParsesRecordedResponse is a contract
// test (CLAUDE.md §20.1): real SDK request-building and
// response-parsing against a recorded fixture, never a live API call
// (CLAUDE.md T3).
func TestOpenAIProvider_Complete_ParsesRecordedResponse(t *testing.T) {
	rec, err := recorder.NewWithOptions(&recorder.Options{
		CassetteName: "testdata/cassettes/openai_complete",
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

	client := openai.NewClient(
		option.WithAPIKey("test-key-not-real"),
		option.WithHTTPClient(rec.GetDefaultClient()),
		option.WithMaxRetries(0),
	)
	p := &OpenAIProvider{client: client, model: "gpt-4o-mini"}

	resp, err := p.Complete(context.Background(), Request{
		System:   "you are a test fixture",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Text != "hello from chatgpt" {
		t.Errorf("Text = %q, want %q", resp.Text, "hello from chatgpt")
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 6 {
		t.Errorf("Usage = %+v, want {InputTokens:12 OutputTokens:6 ...}", resp.Usage)
	}
	if resp.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q, want gpt-4o-mini", resp.Model)
	}
}

func TestOpenAIProvider_Name(t *testing.T) {
	p := NewOpenAIProvider("unused", "gpt-4o-mini")
	if p.Name() != "openai-gpt-4o-mini" {
		t.Errorf("Name() = %q, want openai-gpt-4o-mini", p.Name())
	}
}
