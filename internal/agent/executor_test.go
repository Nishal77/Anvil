package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
)

// fakeTurnStore records every agent_turns insert/update in memory —
// enough to assert "every tool call appears with its policy decision"
// without a real Postgres.
type fakeTurnStore struct {
	mu    sync.Mutex
	turns []storage.AgentTurn
}

func (f *fakeTurnStore) InsertAgentTurn(_ context.Context, t storage.AgentTurn) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.ID = uuid.New()
	f.turns = append(f.turns, t)
	return t.ID, nil
}

func (f *fakeTurnStore) UpdateAgentTurnResult(_ context.Context, id uuid.UUID, observation string, tokensIn, tokensOut int, costUSDMicros int64, latencyMS int, execErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, t := range f.turns {
		if t.ID == id {
			f.turns[i].Observation = observation
			f.turns[i].TokensIn = tokensIn
			f.turns[i].TokensOut = tokensOut
			f.turns[i].CostUSDMicros = costUSDMicros
			f.turns[i].LatencyMS = latencyMS
			f.turns[i].Error = execErr
			return nil
		}
	}
	return errors.New("turn not found")
}

func (f *fakeTurnStore) all() []storage.AgentTurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]storage.AgentTurn(nil), f.turns...)
}

func testExecutor(t *testing.T, provider llm.Provider, sb *fakeSandbox, turns *fakeTurnStore) *Executor {
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
	return &Executor{
		registry: registry, policy: policy, router: router, sandbox: sb,
		idem: newFakeIdemStore(), turns: turns, log: slog.New(slog.DiscardHandler),
		maxTurns: defaultMaxTurnsPerStep, maxObsLen: defaultMaxObservationBytes,
	}
}

func stepDoneCall(t *testing.T, success bool) llm.ToolCall {
	t.Helper()
	args, err := json.Marshal(map[string]any{"summary": "done", "success": success})
	if err != nil {
		t.Fatalf("marshal step_done args: %v", err)
	}
	return llm.ToolCall{ID: "1", Name: "step_done", Input: args}
}

func TestExecutor_RunTurnLoop_StepDoneEndsLoopSuccessfully(t *testing.T) {
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, true)}})
	turns := &fakeTurnStore{}
	e := testExecutor(t, provider, newFakeSandbox(), turns)

	success, summary, err := e.runTurnLoop(context.Background(), uuid.New(), queue.Step{ID: uuid.New(), Idx: 0}, "fake-sandbox", hardcodedPlan[0])
	if err != nil {
		t.Fatalf("runTurnLoop() error = %v", err)
	}
	if !success {
		t.Errorf("success = false, want true")
	}
	if summary != "done" {
		t.Errorf("summary = %q, want %q", summary, "done")
	}
}

func TestExecutor_RunTurnLoop_StepDoneFailureReported(t *testing.T) {
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, false)}})
	e := testExecutor(t, provider, newFakeSandbox(), &fakeTurnStore{})

	success, _, err := e.runTurnLoop(context.Background(), uuid.New(), queue.Step{ID: uuid.New(), Idx: 0}, "fake-sandbox", hardcodedPlan[0])
	if err != nil {
		t.Fatalf("runTurnLoop() error = %v", err)
	}
	if success {
		t.Error("success = true, want false when the model reports step_done(success=false)")
	}
}

// TestExecutor_RunTurnLoop_SchemaInvalidCallSurvivesAsObservation is the
// Week 6 non-negotiable: a schema-invalid tool call must return a
// correctable observation to the model, never fail the job outright.
func TestExecutor_RunTurnLoop_SchemaInvalidCallSurvivesAsObservation(t *testing.T) {
	badArgs := json.RawMessage(`{}`) // fs_read requires "path" — this omits it
	provider := llm.NewFakeProvider("fake").
		ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{{ID: "1", Name: "fs_read", Input: badArgs}}}).
		ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, true)}})
	turns := &fakeTurnStore{}
	e := testExecutor(t, provider, newFakeSandbox(), turns)

	success, _, err := e.runTurnLoop(context.Background(), uuid.New(), queue.Step{ID: uuid.New(), Idx: 0}, "fake-sandbox", hardcodedPlan[0])
	if err != nil {
		t.Fatalf("runTurnLoop() error = %v, want the job to survive a schema-invalid call, not fail", err)
	}
	if !success {
		t.Error("success = false, want true — the loop should recover via the second, valid turn")
	}

	got := turns.all()
	if len(got) != 2 {
		t.Fatalf("recorded %d turns, want 2 (the denied call, then step_done)", len(got))
	}
	if got[0].PolicyDecision != Deny.String() {
		t.Errorf("first turn PolicyDecision = %q, want %q", got[0].PolicyDecision, Deny.String())
	}
	if got[0].Observation == "" {
		t.Error("first turn Observation is empty — the model got nothing to correct")
	}
}

// TestExecutor_RunTurnLoop_TurnLimitExceeded proves FR-021's hard
// ceiling: a model that never calls step_done exhausts MaxTurnsPerStep
// and the step fails cleanly rather than looping forever.
func TestExecutor_RunTurnLoop_TurnLimitExceeded(t *testing.T) {
	provider := llm.NewFakeProvider("fake")
	lsArgs, _ := json.Marshal(map[string]string{"path": "."})
	for i := 0; i < defaultMaxTurnsPerStep; i++ {
		provider.ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{{ID: "1", Name: "fs_list", Input: lsArgs}}})
	}
	e := testExecutor(t, provider, newFakeSandbox(), &fakeTurnStore{})

	_, _, err := e.runTurnLoop(context.Background(), uuid.New(), queue.Step{ID: uuid.New(), Idx: 0}, "fake-sandbox", hardcodedPlan[0])
	if !errors.Is(err, ErrStepTurnLimitExceeded) {
		t.Fatalf("runTurnLoop() error = %v, want ErrStepTurnLimitExceeded", err)
	}
	if provider.Calls() != defaultMaxTurnsPerStep {
		t.Errorf("provider called %d times, want exactly %d (the turn cap)", provider.Calls(), defaultMaxTurnsPerStep)
	}
}

// TestExecutor_RunTurnLoop_NoToolCallPromptsRetry covers a model
// response with zero tool calls: the loop must not crash, and must
// tell the model to try again.
func TestExecutor_RunTurnLoop_NoToolCallPromptsRetry(t *testing.T) {
	provider := llm.NewFakeProvider("fake").
		ScriptResponse(llm.Response{Model: "fake-model", Text: "I'm thinking about it."}).
		ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, true)}})
	e := testExecutor(t, provider, newFakeSandbox(), &fakeTurnStore{})

	success, _, err := e.runTurnLoop(context.Background(), uuid.New(), queue.Step{ID: uuid.New(), Idx: 0}, "fake-sandbox", hardcodedPlan[0])
	if err != nil {
		t.Fatalf("runTurnLoop() error = %v", err)
	}
	if !success {
		t.Error("success = false, want true after the model recovers on turn 2")
	}
}

func TestExecutor_RunTurnLoop_AllowedCallPersistsTurnWithPolicyDecision(t *testing.T) {
	lsArgs, _ := json.Marshal(map[string]string{"path": "."})
	provider := llm.NewFakeProvider("fake").
		ScriptResponse(llm.Response{Model: "fake-model", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}, ToolCalls: []llm.ToolCall{{ID: "1", Name: "fs_list", Input: lsArgs}}}).
		ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, true)}})
	turns := &fakeTurnStore{}
	e := testExecutor(t, provider, newFakeSandbox(), turns)

	if _, _, err := e.runTurnLoop(context.Background(), uuid.New(), queue.Step{ID: uuid.New(), Idx: 0}, "fake-sandbox", hardcodedPlan[0]); err != nil {
		t.Fatalf("runTurnLoop() error = %v", err)
	}

	got := turns.all()
	if len(got) != 2 {
		t.Fatalf("recorded %d turns, want 2", len(got))
	}
	if got[0].ToolName != "fs_list" || got[0].PolicyDecision != Allow.String() {
		t.Errorf("first turn = {tool:%q decision:%q}, want {fs_list, ALLOW}", got[0].ToolName, got[0].PolicyDecision)
	}
	if got[0].TokensIn != 10 || got[0].TokensOut != 5 {
		t.Errorf("first turn tokens = {in:%d out:%d}, want {10, 5}", got[0].TokensIn, got[0].TokensOut)
	}
	if len(got[0].PromptSHA256) == 0 {
		t.Error("PromptSHA256 is empty")
	}
}
