package llm

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// noSleep replaces Router.sleep in retry/failover tests so jittered
// backoff (up to llmBackoffCap per attempt) doesn't burn real
// wall-clock time in the unit test suite.
func noSleep(context.Context, time.Duration) error { return nil }

func TestRouter_Complete_ReturnsPrimaryProviderResponse(t *testing.T) {
	primary := NewFakeProvider("primary").ScriptResponse(Response{Text: "hi", Model: "gemini-2.5-flash"})
	r, err := NewRouter(Config{
		Providers: map[TaskClass][]Provider{TaskPlanning: {primary}},
		Budget:    NewInMemoryBudgetStore(150_000),
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	resp, err := r.Complete(context.Background(), uuid.New(), Request{TaskClass: TaskPlanning})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Text != "hi" {
		t.Fatalf("Complete() = %+v, want Text=hi", resp)
	}
	if primary.Calls() != 1 {
		t.Fatalf("primary called %d times, want 1", primary.Calls())
	}
}

// TestRouter_KillingPrimaryFailsOverWithinOneRequest is Gate 2's own
// wording (BUILD-PLAN W5 exit criteria): a dead primary must not cost
// the caller more than one Complete call before a fallback answers.
func TestRouter_KillingPrimaryFailsOverWithinOneRequest(t *testing.T) {
	primary := NewFakeProvider("primary")
	// Config.MaxRetries follows the repo's <=0-means-default convention
	// (CODE-STANDARDS §4.1), so 0 can't mean "explicitly no retries" —
	// script enough failures to cover the default's worst case
	// (defaultMaxRetries retries + the initial attempt) regardless.
	for i := 0; i < defaultMaxRetries+1; i++ {
		primary.ScriptError(ErrProviderUnavailable)
	}
	fallback := NewFakeProvider("fallback").ScriptResponse(Response{Text: "fallback answered", Model: "claude-haiku-4-5"})
	r, err := NewRouter(Config{
		Providers: map[TaskClass][]Provider{TaskPlanning: {primary, fallback}},
		Budget:    NewInMemoryBudgetStore(150_000),
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	r.sleep = noSleep

	resp, err := r.Complete(context.Background(), uuid.New(), Request{TaskClass: TaskPlanning})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Text != "fallback answered" {
		t.Fatalf("Complete() = %+v, want the fallback's response", resp)
	}
}

// TestRouter_429StormDoesNotOpenBreaker is the non-negotiable from the
// Week 5 prompt: repeated 429s must not disable the provider.
func TestRouter_429StormDoesNotOpenBreaker(t *testing.T) {
	primary := NewFakeProvider("primary")
	fallback := NewFakeProvider("fallback")
	for i := 0; i < 20; i++ {
		primary.ScriptError(ErrRateLimited)
		fallback.ScriptResponse(Response{Text: "ok", Model: "claude-haiku-4-5"})
	}
	r, err := NewRouter(Config{
		Providers:        map[TaskClass][]Provider{TaskPlanning: {primary, fallback}},
		Budget:           NewInMemoryBudgetStore(150_000),
		Logger:           testLogger(),
		BreakerThreshold: 5, // would open after 5 recorded failures, if 429 counted
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	// 20 calls, all 429 on primary — far more than the 5-failure
	// threshold. Every one must still reach primary first (breaker
	// stays closed) and fail over cleanly.
	for i := 0; i < 20; i++ {
		resp, err := r.Complete(context.Background(), uuid.New(), Request{TaskClass: TaskPlanning})
		if err != nil {
			t.Fatalf("call %d: Complete() error = %v", i, err)
		}
		if resp.Text != "ok" {
			t.Fatalf("call %d: Complete() = %+v, want fallback's response", i, resp)
		}
	}
	if primary.Calls() != 20 {
		t.Fatalf("primary called %d times, want 20 — a 429 storm must never open the breaker and skip primary", primary.Calls())
	}
}

func TestRouter_ProviderUnavailable_OpensBreakerAfterThreshold(t *testing.T) {
	const rounds = 10
	primary := NewFakeProvider("primary")
	fallback := NewFakeProvider("fallback")
	// Generous script: covers the worst case of every round retrying
	// the default number of times before failing over, so the test
	// doesn't depend on exactly how many attempts one Complete() call
	// makes against primary before moving on.
	for i := 0; i < rounds*(defaultMaxRetries+1); i++ {
		primary.ScriptError(ErrProviderUnavailable)
	}
	for i := 0; i < rounds; i++ {
		fallback.ScriptResponse(Response{Text: "ok", Model: "claude-haiku-4-5"})
	}
	r, err := NewRouter(Config{
		Providers:        map[TaskClass][]Provider{TaskPlanning: {primary, fallback}},
		Budget:           NewInMemoryBudgetStore(150_000),
		Logger:           testLogger(),
		BreakerThreshold: 5,
		BreakerWindow:    time.Hour, // keep every failure in-window for this test
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	r.sleep = noSleep

	for i := 0; i < rounds; i++ {
		resp, err := r.Complete(context.Background(), uuid.New(), Request{TaskClass: TaskPlanning})
		if err != nil {
			t.Fatalf("call %d: Complete() error = %v", i, err)
		}
		if resp.Text != "ok" {
			t.Fatalf("call %d: Complete() = %+v, want fallback's response", i, resp)
		}
	}

	// If the breaker never opened, every round would drive primary to
	// its worst-case attempt count. Fewer proves at least one round
	// was skipped straight to the fallback.
	worstCase := rounds * (defaultMaxRetries + 1)
	if primary.Calls() >= worstCase {
		t.Fatalf("primary called %d times (worst case %d) — breaker never opened", primary.Calls(), worstCase)
	}
}

func TestRouter_JobBudgetExceeded_FailsBeforeAnyProviderCall(t *testing.T) {
	primary := NewFakeProvider("primary") // no script entries: any call is a test failure
	budget := NewInMemoryBudgetStore(1)   // 1 token budget, any real request estimate exceeds it
	r, err := NewRouter(Config{
		Providers: map[TaskClass][]Provider{TaskPlanning: {primary}},
		Budget:    budget,
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	_, err = r.Complete(context.Background(), uuid.New(), Request{TaskClass: TaskPlanning, System: "a system prompt long enough to blow a 1-token budget"})
	if !errors.Is(err, ErrJobBudgetExceeded) {
		t.Fatalf("Complete() error = %v, want ErrJobBudgetExceeded", err)
	}
	if primary.Calls() != 0 {
		t.Fatalf("primary called %d times, want 0 — budget must be checked before touching any provider", primary.Calls())
	}
}

func TestRouter_GlobalCapExceeded_FailsBeforeAnyProviderCall(t *testing.T) {
	primary := NewFakeProvider("primary")
	r, err := NewRouter(Config{
		Providers: map[TaskClass][]Provider{TaskPlanning: {primary}},
		Budget:    NewInMemoryBudgetStore(150_000),
		Cap:       NewGlobalCap(&fakeSpendReader{spentUSDMicros: 10_000_000}, 10_000_000, testLogger()),
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	_, err = r.Complete(context.Background(), uuid.New(), Request{TaskClass: TaskPlanning})
	if !errors.Is(err, ErrGlobalCapExceeded) {
		t.Fatalf("Complete() error = %v, want ErrGlobalCapExceeded", err)
	}
	if primary.Calls() != 0 {
		t.Fatalf("primary called %d times, want 0", primary.Calls())
	}
}

func TestRouter_AllProvidersExhausted(t *testing.T) {
	primary := NewFakeProvider("primary")
	for i := 0; i < defaultMaxRetries+1; i++ {
		primary.ScriptError(ErrProviderUnavailable)
	}
	r, err := NewRouter(Config{
		Providers: map[TaskClass][]Provider{TaskPlanning: {primary}},
		Budget:    NewInMemoryBudgetStore(150_000),
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	r.sleep = noSleep

	_, err = r.Complete(context.Background(), uuid.New(), Request{TaskClass: TaskPlanning})
	if !errors.Is(err, ErrAllProvidersExhausted) {
		t.Fatalf("Complete() error = %v, want ErrAllProvidersExhausted", err)
	}
}

func TestRouter_FatalError_DoesNotFailOverOrRetry(t *testing.T) {
	primary := NewFakeProvider("primary").ScriptError(ErrProviderFatal)
	fallback := NewFakeProvider("fallback").ScriptResponse(Response{Text: "should never be reached"})
	r, err := NewRouter(Config{
		Providers:  map[TaskClass][]Provider{TaskPlanning: {primary, fallback}},
		Budget:     NewInMemoryBudgetStore(150_000),
		Logger:     testLogger(),
		MaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	_, err = r.Complete(context.Background(), uuid.New(), Request{TaskClass: TaskPlanning})
	if err == nil {
		t.Fatal("Complete() error = nil, want a wrapped ErrProviderFatal")
	}
	if primary.Calls() != 1 {
		t.Fatalf("primary called %d times, want 1 — a fatal error must not be retried", primary.Calls())
	}
	if fallback.Calls() != 0 {
		t.Fatalf("fallback called %d times, want 0 — a fatal error must not fail over", fallback.Calls())
	}
}

func TestRouter_SuccessfulCallRecordsUsageInBudgetStore(t *testing.T) {
	primary := NewFakeProvider("primary").ScriptResponse(Response{
		Model: "claude-haiku-4-5",
		Usage: Usage{InputTokens: 100, OutputTokens: 50},
	})
	budget := NewInMemoryBudgetStore(150_000)
	r, err := NewRouter(Config{
		Providers: map[TaskClass][]Provider{TaskPlanning: {primary}},
		Budget:    budget,
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	jobID := uuid.New()
	if _, err := r.Complete(context.Background(), jobID, Request{TaskClass: TaskPlanning}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	got, err := budget.GetJobBudget(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJobBudget() error = %v", err)
	}
	if got.TokensUsed != 150 {
		t.Fatalf("TokensUsed = %d, want 150 (100 in + 50 out)", got.TokensUsed)
	}
}
