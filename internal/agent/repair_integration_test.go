package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
)

// seedJobWithOneStep seeds a real QUEUED job with one step whose
// acceptance/optional the planner would have written via SavePlan —
// CreateQueuedJob + EnsureStep alone can't set those columns.
func seedJobWithOneStep(t *testing.T, pool *pgxpool.Pool, optional bool) *queue.Job {
	t.Helper()
	job := seedQueuedJobWithSteps(t, pool) // seeds testPlanStepCount (3) plain steps
	_, err := pool.Exec(context.Background(),
		`UPDATE steps SET acceptance = 'go test passes', optional = $2 WHERE job_id = $1 AND idx = 0`,
		job.ID, optional)
	if err != nil {
		t.Fatalf("seed step acceptance/optional: %v", err)
	}
	// Only step 0 is exercised by these tests; mark the rest done so
	// RunStep doesn't run out of scripted turns on them.
	steps, err := queue.ListSteps(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	for _, s := range steps[1:] {
		if err := queue.FinishStep(context.Background(), pool, s.ID, queue.StepSucceeded, ""); err != nil {
			t.Fatalf("FinishStep: %v", err)
		}
	}
	return job
}

func execCall(t *testing.T, command string) llm.ToolCall {
	t.Helper()
	args, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatalf("marshal exec args: %v", err)
	}
	return llm.ToolCall{ID: "1", Name: "exec", Input: args}
}

// TestFR022_RepairLoopRecoversFromForcedFailure forces the first exec
// call to fail, then lets the second succeed — proving the repair loop
// (not luck) is what gets the step to SUCCEEDED.
func TestFR022_RepairLoopRecoversFromForcedFailure(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedJobWithOneStep(t, pool, false)

	sb := newFakeSandbox()
	callCount := 0
	sb.execFunc = func(string) (string, string, int) {
		callCount++
		if callCount == 1 {
			return "", "compile error", 1
		}
		return "ok", "", 0
	}

	// Distinct commands per attempt: identical args would hit the
	// idempotency cache (I-2) and replay the first failure's result
	// instead of actually re-executing — exactly what a diagnose-then-act
	// repair should never do anyway.
	provider := llm.NewFakeProvider("fake").
		ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{execCall(t, "go build ./...")}}).
		ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{execCall(t, "go build -v ./...")}}).
		ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, true)}})
	exec := newIntegrationExecutor(t, pool, sb, provider)

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep() error = %v, want the repair loop to recover", err)
	}

	steps, err := queue.ListSteps(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if steps[0].Status != queue.StepSucceeded {
		t.Errorf("step 0 status = %q, want SUCCEEDED", steps[0].Status)
	}
	if steps[0].RepairCount != 1 {
		t.Errorf("step 0 RepairCount = %d, want 1", steps[0].RepairCount)
	}
}

// TestFR022_RepairCapEndsStepCleanly forces every exec call to fail —
// the step must end FAILED after MaxRepairsPerStep attempts, not hang
// the job waiting for a repair that will never land.
func TestFR022_RepairCapEndsStepCleanly(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedJobWithOneStep(t, pool, false)

	sb := newFakeSandbox()
	sb.execFunc = func(string) (string, string, int) { return "", "still broken", 1 }

	provider := llm.NewFakeProvider("fake")
	for range defaultMaxTurnsPerStep {
		provider.ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{execCall(t, "go build ./...")}})
	}
	exec := newIntegrationExecutor(t, pool, sb, provider)

	if err := exec.RunStep(context.Background(), job); err == nil {
		t.Fatal("RunStep() error = nil, want an error — every repair attempt failed")
	}

	steps, err := queue.ListSteps(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if steps[0].Status != queue.StepFailed {
		t.Errorf("step 0 status = %q, want FAILED", steps[0].Status)
	}
	if steps[0].RepairCount != defaultMaxRepairsPerStep+1 {
		t.Errorf("step 0 RepairCount = %d, want %d (the cap plus the attempt that exceeded it)", steps[0].RepairCount, defaultMaxRepairsPerStep+1)
	}
}

// TestFR022_RepairCountSurvivesReclaim proves repair_count is read
// from the row, not from in-memory state: a step already at the repair
// cap (as if a prior worker crash-repair-crashed up to it) fails on
// its very next exec failure, without needing 3 more attempts.
func TestFR022_RepairCountSurvivesReclaim(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedJobWithOneStep(t, pool, false)
	steps, err := queue.ListSteps(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE steps SET repair_count = $2 WHERE id = $1`, steps[0].ID, defaultMaxRepairsPerStep); err != nil {
		t.Fatalf("preset repair_count: %v", err)
	}

	sb := newFakeSandbox()
	sb.execFunc = func(string) (string, string, int) { return "", "broken", 1 }
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{execCall(t, "go build ./...")}})
	exec := newIntegrationExecutor(t, pool, sb, provider)

	if err := exec.RunStep(context.Background(), job); err == nil {
		t.Fatal("RunStep() error = nil, want the step to fail immediately — repair_count was already at the cap")
	}
	if provider.Calls() != 1 {
		t.Errorf("provider called %d times, want exactly 1 — the cap must be read from the row, not re-earned in memory", provider.Calls())
	}
}

// TestFR022_OptionalStepFailureSkipsNotFails proves an Optional step
// that exhausts its repairs lands SKIPPED, and the job continues
// instead of failing outright.
func TestFR022_OptionalStepFailureSkipsNotFails(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedJobWithOneStep(t, pool, true)

	sb := newFakeSandbox()
	sb.execFunc = func(string) (string, string, int) { return "", "broken", 1 }
	provider := llm.NewFakeProvider("fake")
	for range defaultMaxTurnsPerStep {
		provider.ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{execCall(t, "go build ./...")}})
	}
	exec := newIntegrationExecutor(t, pool, sb, provider)

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep() error = %v, want nil — an optional step's failure must not fail the job", err)
	}

	steps, err := queue.ListSteps(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if steps[0].Status != queue.StepSkipped {
		t.Errorf("step 0 status = %q, want SKIPPED", steps[0].Status)
	}
}
