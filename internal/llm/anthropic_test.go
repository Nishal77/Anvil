package llm

import (
	"context"
	"net/http"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"gopkg.in/dnaeon/go-vcr.v3/cassette"
	"gopkg.in/dnaeon/go-vcr.v3/recorder"
)

// TestAnthropicProvider_Complete_ParsesRecordedResponse is a contract
// test (CLAUDE.md §20.1): it drives the real SDK request-building and
// response-parsing path against a recorded fixture, never a live API
// call (CLAUDE.md T3). The cassette's Authorization-shaped header is
// already a scrubbed placeholder — see testdata/cassettes/README.md
// for the rule applied before any real recording is committed.
func TestAnthropicProvider_Complete_ParsesRecordedResponse(t *testing.T) {
	rec, err := recorder.NewWithOptions(&recorder.Options{
		CassetteName: "testdata/cassettes/anthropic_complete",
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
	// Match on method only: the exact URL the SDK builds is an
	// internal implementation detail this test does not pin down: it
	// exists to prove response parsing, not to lock in a private
	// request-shape contract.
	rec.SetMatcher(func(r *http.Request, i cassette.Request) bool {
		return r.Method == i.Method
	})

	client := anthropic.NewClient(
		option.WithAPIKey("test-key-not-real"),
		option.WithHTTPClient(rec.GetDefaultClient()),
		option.WithMaxRetries(0),
	)
	p := &AnthropicProvider{client: client, model: anthropic.ModelClaudeHaiku4_5}

	resp, err := p.Complete(context.Background(), Request{
		System:   "you are a test fixture",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Text != "hello from claude" {
		t.Errorf("Text = %q, want %q", resp.Text, "hello from claude")
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want {InputTokens:10 OutputTokens:5 ...}", resp.Usage)
	}
	if resp.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want claude-haiku-4-5", resp.Model)
	}
}
