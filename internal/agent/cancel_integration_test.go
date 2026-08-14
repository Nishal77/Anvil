package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
)

// countingCancelWatcher reports cancelled after cancelAfter calls —
// enough calls to have started the step but not enough to have
// finished it on the happy path, simulating a POST /cancel that lands
// mid-step rather than before the step even starts.
type countingCancelWatcher struct {
	calls       int
	cancelAfter int
}

func (w *countingCancelWatcher) Cancelled(context.Context, uuid.UUID) (bool, error) {
	w.calls++
	return w.calls > w.cancelAfter, nil
}

// TestUS04_CancelCheckedBetweenTurnsNotOnlySteps proves cancellation
// observed mid-step stops the loop within that same step — not only at
// the next step boundary, which for a long step could be minutes away.
func TestUS04_CancelCheckedBetweenTurnsNotOnlySteps(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedQueuedJobWithSteps(t, pool)

	sb := newFakeSandbox()
	provider := llm.NewFakeProvider("fake")
	for range defaultMaxTurnsPerStep {
		provider.ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{execCall(t, "true")}})
	}

	watcher := &countingCancelWatcher{cancelAfter: 2}
	exec := newIntegrationExecutor(t, pool, sb, provider)
	exec.cancel = watcher

	err := exec.RunStep(context.Background(), job)
	if err != nil {
		t.Fatalf("RunStep() error = %v, want nil — a cancelled job finishes cleanly, not as a failure", err)
	}

	got, err := queue.GetJob(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != queue.StatusCancelled {
		t.Errorf("Status = %s, want CANCELLED", got.Status)
	}
	// The provider was scripted for defaultMaxTurnsPerStep turns; far
	// fewer must actually have been used, proving the check fired
	// between turns rather than only once the script ran out.
	if provider.Calls() >= defaultMaxTurnsPerStep {
		t.Errorf("provider called %d times, want well under %d — cancellation must stop the loop mid-step", provider.Calls(), defaultMaxTurnsPerStep)
	}
	if sb.destroyed != 1 {
		t.Errorf("sandbox destroyed %d times, want 1", sb.destroyed)
	}
}

// TestUS04_CancelBetweenStepsStillReachesCancelled proves the
// step-boundary check (runAllSteps, not just runTurnLoop) also works:
// cancellation observed before a step even starts still ends the job
// in CANCELLED.
func TestUS04_CancelBetweenStepsStillReachesCancelled(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedQueuedJobWithSteps(t, pool)

	sb := newFakeSandbox()
	exec := newIntegrationExecutor(t, pool, sb, llm.NewFakeProvider("fake"))
	exec.cancel = &countingCancelWatcher{cancelAfter: 0}

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep() error = %v, want nil", err)
	}

	got, err := queue.GetJob(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != queue.StatusCancelled {
		t.Errorf("Status = %s, want CANCELLED", got.Status)
	}
}
