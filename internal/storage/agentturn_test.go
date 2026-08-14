package storage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// seedStepForAgentTurns inserts a user, job, and step so agent_turns'
// foreign keys have something to reference, and returns (jobID, stepID).
func seedStepForAgentTurns(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	jobID := seedJobForEvents(t)

	var stepID uuid.UUID
	err := testStore.pool.QueryRow(ctx,
		`INSERT INTO steps (job_id, idx, title, description) VALUES ($1, 0, 'title', 'description') RETURNING id`,
		jobID,
	).Scan(&stepID)
	if err != nil {
		t.Fatalf("seed step: %v", err)
	}
	return jobID, stepID
}

func TestInsertAgentTurn_ListAgentTurns_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	jobID, stepID := seedStepForAgentTurns(t)

	id, err := testStore.InsertAgentTurn(ctx, AgentTurn{
		JobID: jobID, StepID: stepID, TurnIdx: 0, Role: "executor",
		Model: "claude-haiku-4-5", Provider: "anthropic",
		PromptSHA256:   []byte{1, 2, 3, 4},
		ToolName:       "fs_list",
		ToolArgs:       json.RawMessage(`{"path":"."}`),
		PolicyDecision: "ALLOW",
	})
	if err != nil {
		t.Fatalf("InsertAgentTurn() error = %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("InsertAgentTurn() returned a zero-value ID")
	}

	got, err := testStore.ListAgentTurns(ctx, jobID)
	if err != nil {
		t.Fatalf("ListAgentTurns() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListAgentTurns() returned %d turns, want 1", len(got))
	}
	if got[0].ToolName != "fs_list" || got[0].PolicyDecision != "ALLOW" {
		t.Errorf("ListAgentTurns()[0] = {tool:%q decision:%q}, want {fs_list, ALLOW}", got[0].ToolName, got[0].PolicyDecision)
	}
	if got[0].StepID != stepID {
		t.Errorf("ListAgentTurns()[0].StepID = %v, want %v", got[0].StepID, stepID)
	}
}

func TestUpdateAgentTurnResult_FillsInExecutionOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	jobID, stepID := seedStepForAgentTurns(t)

	id, err := testStore.InsertAgentTurn(ctx, AgentTurn{
		JobID: jobID, StepID: stepID, TurnIdx: 0, Role: "executor",
		Model: "gemini-2.5-flash", Provider: "gemini",
		PromptSHA256:   []byte{9, 9, 9},
		ToolName:       "exec",
		PolicyDecision: "ALLOW",
	})
	if err != nil {
		t.Fatalf("InsertAgentTurn() error = %v", err)
	}

	if err := testStore.UpdateAgentTurnResult(ctx, id, "exit code: 0", 100, 20, 5, 250, ""); err != nil {
		t.Fatalf("UpdateAgentTurnResult() error = %v", err)
	}

	got, err := testStore.ListAgentTurns(ctx, jobID)
	if err != nil {
		t.Fatalf("ListAgentTurns() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListAgentTurns() returned %d turns, want 1", len(got))
	}
	if got[0].Observation != "exit code: 0" {
		t.Errorf("Observation = %q, want %q", got[0].Observation, "exit code: 0")
	}
	if got[0].TokensIn != 100 || got[0].TokensOut != 20 {
		t.Errorf("tokens = {in:%d out:%d}, want {100, 20}", got[0].TokensIn, got[0].TokensOut)
	}
	if got[0].CostUSDMicros != 5 {
		t.Errorf("CostUSDMicros = %d, want 5", got[0].CostUSDMicros)
	}
}

func TestListAgentTurns_OrderedByTurnIdx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	jobID, stepID := seedStepForAgentTurns(t)

	for _, idx := range []int{2, 0, 1} { // inserted out of order
		if _, err := testStore.InsertAgentTurn(ctx, AgentTurn{
			JobID: jobID, StepID: stepID, TurnIdx: idx, Role: "executor",
			Model: "m", Provider: "p", PromptSHA256: []byte{byte(idx)}, PolicyDecision: "ALLOW",
		}); err != nil {
			t.Fatalf("InsertAgentTurn(idx=%d) error = %v", idx, err)
		}
	}

	got, err := testStore.ListAgentTurns(ctx, jobID)
	if err != nil {
		t.Fatalf("ListAgentTurns() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListAgentTurns() returned %d turns, want 3", len(got))
	}
	for i, turn := range got {
		if turn.TurnIdx != i {
			t.Errorf("ListAgentTurns()[%d].TurnIdx = %d, want %d", i, turn.TurnIdx, i)
		}
	}
}

func TestListAgentTurns_SeparateJobsDoNotInterfere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	jobA, stepA := seedStepForAgentTurns(t)
	jobB, stepB := seedStepForAgentTurns(t)

	if _, err := testStore.InsertAgentTurn(ctx, AgentTurn{JobID: jobA, StepID: stepA, Role: "executor", Model: "m", Provider: "p", PromptSHA256: []byte{1}, PolicyDecision: "ALLOW"}); err != nil {
		t.Fatalf("InsertAgentTurn(jobA) error = %v", err)
	}
	if _, err := testStore.InsertAgentTurn(ctx, AgentTurn{JobID: jobB, StepID: stepB, Role: "executor", Model: "m", Provider: "p", PromptSHA256: []byte{2}, PolicyDecision: "ALLOW"}); err != nil {
		t.Fatalf("InsertAgentTurn(jobB) error = %v", err)
	}

	got, err := testStore.ListAgentTurns(ctx, jobA)
	if err != nil {
		t.Fatalf("ListAgentTurns() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListAgentTurns(jobA) returned %d turns, want 1 (jobB's turn must not leak in)", len(got))
	}
}
