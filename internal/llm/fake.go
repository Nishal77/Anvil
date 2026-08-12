package llm

import (
	"context"
	"fmt"
	"sync"
)

// FakeProvider replays a scripted sequence of Response/error pairs —
// one per Complete call, in order. It never touches a network. Used
// by every test in this repo that would otherwise call a real LLM API
// (CLAUDE.md T3): the Router, budget, and benchmark harness tests all
// drive this instead of Gemini or Anthropic.
type FakeProvider struct {
	name string

	mu      sync.Mutex
	script  []scriptedCall
	callIdx int
}

type scriptedCall struct {
	resp Response
	err  error
}

// NewFakeProvider constructs a FakeProvider identified by name (for
// breaker/metric labeling in tests that exercise multi-provider
// failover).
func NewFakeProvider(name string) *FakeProvider {
	return &FakeProvider{name: name}
}

// ScriptResponse appends a successful response to the end of the
// script.
func (f *FakeProvider) ScriptResponse(resp Response) *FakeProvider {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, scriptedCall{resp: resp})
	return f
}

// ScriptError appends a failing call to the end of the script. Pass
// one of this package's sentinel errors (ErrRateLimited,
// ErrProviderUnavailable, ...) so Router's classification logic has
// something real to branch on.
func (f *FakeProvider) ScriptError(err error) *FakeProvider {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, scriptedCall{err: err})
	return f
}

// Complete returns the next scripted call. Calling it more times than
// the script has entries is a test-setup bug, reported as an error
// rather than a panic or an out-of-bounds silently repeating the last
// entry.
func (f *FakeProvider) Complete(_ context.Context, _ Request) (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.callIdx >= len(f.script) {
		return Response{}, fmt.Errorf("llm: fake provider %s: script exhausted after %d calls", f.name, f.callIdx)
	}
	call := f.script[f.callIdx]
	f.callIdx++
	return call.resp, call.err
}

// Name identifies this fake for breaker state and metric labels.
func (f *FakeProvider) Name() string { return f.name }

// Calls reports how many times Complete has been invoked, for tests
// asserting a specific retry/failover count occurred.
func (f *FakeProvider) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callIdx
}
