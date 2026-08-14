package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
)

// This file covers Executor.RunStep and everything it drives
// (ensureSandbox, runAllSteps, runOneStep) against a REAL Postgres —
// those functions call straight into internal/queue's pool-based
// helpers, which have no interface to fake. The sandbox and LLM stay
// fake (fakeSandbox, llm.FakeProvider): CLAUDE.md T3/T5 forbid a real
// LLM call in any test and don't require Docker for logic that's
// already proven against a real container in test/integration. See
// main_test.go for the shared Postgres container this file's tests
// use via requireIntegrationPool.

type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, uuid.UUID, storage.EventType, json.RawMessage) error {
	return nil
}

func seedTestUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		uuid.NewString()+"@example.com",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func newIntegrationExecutor(t *testing.T, pool *pgxpool.Pool, sb *fakeSandbox, provider llm.Provider) *Executor {
	t.Helper()
	registry := testRegistry(t, sb)
	budget := llm.NewInMemoryBudgetStore(150_000)
	router, err := llm.NewRouter(llm.Config{
		Providers: map[llm.TaskClass][]llm.Provider{llm.TaskExecution: {provider}},
		Budget:    budget,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("llm.NewRouter() error = %v", err)
	}
	policy, err := NewPolicyEngine(PolicyEngineConfig{Registry: registry, Sandbox: sb, Budget: budget})
	if err != nil {
		t.Fatalf("NewPolicyEngine() error = %v", err)
	}
	exec, err := New(Config{
		Registry: registry, Policy: policy, Router: router, Sandbox: sb,
		Publisher: noopPublisher{}, IdemStore: newFakeIdemStore(), Turns: &fakeTurnStore{},
		Pool: pool, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return exec
}

func stepDoneOnceProvider(t *testing.T) *llm.FakeProvider {
	t.Helper()
	p := llm.NewFakeProvider("fake")
	for range 3 {
		p.ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, true)}})
	}
	return p
}

func TestExecutor_RunStep_AllStepsSucceed(t *testing.T) {
	pool := requireIntegrationPool(t)
	userID := seedTestUser(t, pool)
	job, err := queue.CreateQueuedJob(context.Background(), pool, userID, "build me a thing")
	if err != nil {
		t.Fatalf("CreateQueuedJob() error = %v", err)
	}

	sb := newFakeSandbox()
	exec := newIntegrationExecutor(t, pool, sb, stepDoneOnceProvider(t))

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep() error = %v", err)
	}

	gotJob, err := queue.GetJob(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("GetJob() error = %v", err)
	}
	if gotJob.SandboxID == "" {
		t.Error("job has no sandbox id recorded")
	}
	if sb.created != 1 || sb.destroyed != 1 {
		t.Errorf("sandbox created=%d destroyed=%d, want 1 and 1", sb.created, sb.destroyed)
	}

	for idx := range hardcodedPlan {
		step, err := queue.EnsureStep(context.Background(), pool, job.ID, idx, "x", "x")
		if err != nil {
			t.Fatalf("EnsureStep(%d) error = %v", idx, err)
		}
		if step.Status != queue.StepSucceeded {
			t.Errorf("step %d status = %q, want %q", idx, step.Status, queue.StepSucceeded)
		}
	}
}

func TestExecutor_RunStep_ResumesFromLastSucceededStep(t *testing.T) {
	pool := requireIntegrationPool(t)
	userID := seedTestUser(t, pool)
	job, err := queue.CreateQueuedJob(context.Background(), pool, userID, "build me a thing")
	if err != nil {
		t.Fatalf("CreateQueuedJob() error = %v", err)
	}

	// Manually mark step 0 SUCCEEDED, as if a prior, crashed attempt had
	// already finished it — RunStep must not redo it.
	step0, err := queue.EnsureStep(context.Background(), pool, job.ID, 0, hardcodedPlan[0].Title, hardcodedPlan[0].Description)
	if err != nil {
		t.Fatalf("EnsureStep(0) error = %v", err)
	}
	if err := queue.StartStep(context.Background(), pool, step0.ID); err != nil {
		t.Fatalf("StartStep(0) error = %v", err)
	}
	if err := queue.FinishStep(context.Background(), pool, step0.ID, queue.StepSucceeded, ""); err != nil {
		t.Fatalf("FinishStep(0) error = %v", err)
	}

	sb := newFakeSandbox()
	// Only 2 scripted turns: if RunStep tried to redo step 0, the
	// FakeProvider would run out of script and the test would fail
	// with a script-exhaustion error instead of completing.
	provider := llm.NewFakeProvider("fake")
	provider.ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, true)}})
	provider.ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, true)}})
	exec := newIntegrationExecutor(t, pool, sb, provider)

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep() error = %v", err)
	}
	if provider.Calls() != 2 {
		t.Errorf("provider called %d times, want 2 — step 0 must have been skipped, not redone", provider.Calls())
	}
}

func TestExecutor_RunStep_StepFailurePropagatesAndStillDestroysSandbox(t *testing.T) {
	pool := requireIntegrationPool(t)
	userID := seedTestUser(t, pool)
	job, err := queue.CreateQueuedJob(context.Background(), pool, userID, "build me a thing")
	if err != nil {
		t.Fatalf("CreateQueuedJob() error = %v", err)
	}

	sb := newFakeSandbox()
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, false)}})
	exec := newIntegrationExecutor(t, pool, sb, provider)

	if err := exec.RunStep(context.Background(), job); err == nil {
		t.Fatal("RunStep() error = nil, want an error when the model reports step_done(success=false)")
	}
	// The sandbox IS destroyed even on a step failure — only a
	// cancelled ctx (process shutdown) skips it, not a real failure.
	if sb.destroyed != 1 {
		t.Errorf("sandbox destroyed %d times, want 1", sb.destroyed)
	}

	step0, err := queue.EnsureStep(context.Background(), pool, job.ID, 0, "x", "x")
	if err != nil {
		t.Fatalf("EnsureStep(0) error = %v", err)
	}
	if step0.Status != queue.StepFailed {
		t.Errorf("step 0 status = %q, want %q", step0.Status, queue.StepFailed)
	}
}

func TestConfig_SetDefaults(t *testing.T) {
	cfg := Config{}
	cfg.setDefaults()
	if cfg.MaxTurnsPerStep != defaultMaxTurnsPerStep {
		t.Errorf("MaxTurnsPerStep = %d, want %d", cfg.MaxTurnsPerStep, defaultMaxTurnsPerStep)
	}
	if cfg.MaxObservationBytes != defaultMaxObservationBytes {
		t.Errorf("MaxObservationBytes = %d, want %d", cfg.MaxObservationBytes, defaultMaxObservationBytes)
	}
}

func TestConfig_Validate_MissingFields(t *testing.T) {
	if err := (Config{}).validate(); err == nil {
		t.Fatal("validate() error = nil, want an error for a completely empty Config")
	}
}

func TestNew_RejectsInvalidConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New() error = nil, want an error for a completely empty Config")
	}
}

func TestNew_ValidConfig(t *testing.T) {
	pool := requireIntegrationPool(t)
	sb := newFakeSandbox()
	exec := newIntegrationExecutor(t, pool, sb, llm.NewFakeProvider("fake"))
	if exec == nil {
		t.Fatal("New() returned nil with no error")
	}
}

func TestProviderFromModel(t *testing.T) {
	tests := map[string]string{
		"gemini-2.5-flash": "gemini",
		"claude-haiku-4-5": "anthropic",
		"some-other-model": "unknown",
	}
	for model, want := range tests {
		if got := providerFromModel(model); got != want {
			t.Errorf("providerFromModel(%q) = %q, want %q", model, got, want)
		}
	}
}
