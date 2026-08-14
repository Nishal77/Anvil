package queue

import (
	"context"
	"testing"
)

func TestStep_EnsureStep_IsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job := seedQueuedJob(t)

	first, err := EnsureStep(ctx, testPool, job.ID, 0, "Do the thing", "a description")
	if err != nil {
		t.Fatalf("EnsureStep: %v", err)
	}
	if first.Status != StepPending {
		t.Errorf("Status = %q, want %q", first.Status, StepPending)
	}

	if err := StartStep(ctx, testPool, first.ID); err != nil {
		t.Fatalf("StartStep: %v", err)
	}

	// A second EnsureStep call for the same (job, idx) — as happens when a
	// worker crashes and a fresh worker resumes the job — must not create
	// a duplicate row or reset progress already made on this step.
	second, err := EnsureStep(ctx, testPool, job.ID, 0, "Do the thing", "a description")
	if err != nil {
		t.Fatalf("EnsureStep (second call): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second EnsureStep created a new row (%s), want the same one (%s)", second.ID, first.ID)
	}
	if second.Status != StepRunning {
		t.Errorf("Status = %q, want %q — EnsureStep must not reset progress", second.Status, StepRunning)
	}
}

func TestStep_FinishStep_SetsStatusAndError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job := seedQueuedJob(t)

	step, err := EnsureStep(ctx, testPool, job.ID, 0, "title", "description")
	if err != nil {
		t.Fatalf("EnsureStep: %v", err)
	}
	if err := StartStep(ctx, testPool, step.ID); err != nil {
		t.Fatalf("StartStep: %v", err)
	}
	if err := FinishStep(ctx, testPool, step.ID, StepFailed, "boom"); err != nil {
		t.Fatalf("FinishStep: %v", err)
	}

	got, err := EnsureStep(ctx, testPool, job.ID, 0, "title", "description")
	if err != nil {
		t.Fatalf("EnsureStep (read back): %v", err)
	}
	if got.Status != StepFailed {
		t.Errorf("Status = %q, want %q", got.Status, StepFailed)
	}
	if got.Error != "boom" {
		t.Errorf("Error = %q, want %q", got.Error, "boom")
	}
	if got.FinishedAt == nil {
		t.Error("FinishedAt is nil, want set")
	}
}

func TestStep_IncrementRepairCount_AccumulatesAndReturnsNewValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job := seedQueuedJob(t)
	step, err := EnsureStep(ctx, testPool, job.ID, 0, "title", "description")
	if err != nil {
		t.Fatalf("EnsureStep: %v", err)
	}

	for want := 1; want <= 3; want++ {
		got, err := IncrementRepairCount(ctx, testPool, step.ID)
		if err != nil {
			t.Fatalf("IncrementRepairCount: %v", err)
		}
		if got != want {
			t.Errorf("IncrementRepairCount() = %d, want %d", got, want)
		}
	}

	reread, err := EnsureStep(ctx, testPool, job.ID, 0, "title", "description")
	if err != nil {
		t.Fatalf("EnsureStep (read back): %v", err)
	}
	if reread.RepairCount != 3 {
		t.Errorf("RepairCount = %d, want 3 — must be read from the row, not memory", reread.RepairCount)
	}
}

func TestStep_IncrementTurnCount_Accumulates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job := seedQueuedJob(t)
	step, err := EnsureStep(ctx, testPool, job.ID, 0, "title", "description")
	if err != nil {
		t.Fatalf("EnsureStep: %v", err)
	}

	for range 4 {
		if err := IncrementTurnCount(ctx, testPool, step.ID); err != nil {
			t.Fatalf("IncrementTurnCount: %v", err)
		}
	}

	reread, err := EnsureStep(ctx, testPool, job.ID, 0, "title", "description")
	if err != nil {
		t.Fatalf("EnsureStep (read back): %v", err)
	}
	if reread.TurnCount != 4 {
		t.Errorf("TurnCount = %d, want 4", reread.TurnCount)
	}
}

func TestStep_ListSteps_ReturnsInIdxOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job := seedQueuedJob(t)
	for _, idx := range []int{2, 0, 1} {
		if _, err := EnsureStep(ctx, testPool, job.ID, idx, "title", "description"); err != nil {
			t.Fatalf("EnsureStep(%d): %v", idx, err)
		}
	}

	steps, err := ListSteps(ctx, testPool, job.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("len(steps) = %d, want 3", len(steps))
	}
	for i, s := range steps {
		if s.Idx != i {
			t.Errorf("steps[%d].Idx = %d, want %d — ListSteps must return idx order", i, s.Idx, i)
		}
	}
}

func TestStep_EnsureStep_DifferentIdxAreIndependent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job := seedQueuedJob(t)

	step0, err := EnsureStep(ctx, testPool, job.ID, 0, "first", "d")
	if err != nil {
		t.Fatalf("EnsureStep(0): %v", err)
	}
	step1, err := EnsureStep(ctx, testPool, job.ID, 1, "second", "d")
	if err != nil {
		t.Fatalf("EnsureStep(1): %v", err)
	}
	if step0.ID == step1.ID {
		t.Fatal("EnsureStep(0) and EnsureStep(1) returned the same row")
	}
	if err := FinishStep(ctx, testPool, step0.ID, StepSucceeded, ""); err != nil {
		t.Fatalf("FinishStep(step0): %v", err)
	}

	gotStep1, err := EnsureStep(ctx, testPool, job.ID, 1, "second", "d")
	if err != nil {
		t.Fatalf("EnsureStep(1) (read back): %v", err)
	}
	if gotStep1.Status != StepPending {
		t.Errorf("step 1 Status = %q, want %q — finishing step 0 must not affect it", gotStep1.Status, StepPending)
	}
}
