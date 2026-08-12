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
