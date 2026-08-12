package llm

import (
	"context"
	"testing"
)

func TestFakeProvider_ReplaysScriptInOrder(t *testing.T) {
	p := NewFakeProvider("fake").
		ScriptResponse(Response{Text: "first"}).
		ScriptError(ErrRateLimited).
		ScriptResponse(Response{Text: "third"})

	resp, err := p.Complete(context.Background(), Request{})
	if err != nil || resp.Text != "first" {
		t.Fatalf("call 1 = (%+v, %v), want (first, nil)", resp, err)
	}

	_, err = p.Complete(context.Background(), Request{})
	if err != ErrRateLimited {
		t.Fatalf("call 2 error = %v, want ErrRateLimited", err)
	}

	resp, err = p.Complete(context.Background(), Request{})
	if err != nil || resp.Text != "third" {
		t.Fatalf("call 3 = (%+v, %v), want (third, nil)", resp, err)
	}
}

func TestFakeProvider_ExhaustedScriptReturnsError(t *testing.T) {
	p := NewFakeProvider("fake").ScriptResponse(Response{Text: "only"})
	if _, err := p.Complete(context.Background(), Request{}); err != nil {
		t.Fatalf("call 1 error = %v, want nil", err)
	}
	if _, err := p.Complete(context.Background(), Request{}); err == nil {
		t.Fatal("call 2 (past end of script) error = nil, want an error")
	}
}

func TestFakeProvider_Name(t *testing.T) {
	p := NewFakeProvider("gemini")
	if p.Name() != "gemini" {
		t.Fatalf("Name() = %q, want gemini", p.Name())
	}
}
