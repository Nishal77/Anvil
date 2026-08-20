package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedFailedJobWithFailedStep seeds a job that ran, had one step fail,
// and landed in FAILED — the shape RetryJob expects.
func seedFailedJobWithFailedStep(t *testing.T) *Job {
	t.Helper()
	ctx := context.Background()
	job := seedQueuedJob(t)

	if _, err := testPool.Exec(ctx, `UPDATE jobs SET status = 'RUNNING' WHERE id = $1`, job.ID); err != nil {
		t.Fatalf("set job running: %v", err)
	}
	step, err := EnsureStep(ctx, testPool, job.ID, 0, "title", "description")
	if err != nil {
		t.Fatalf("EnsureStep: %v", err)
	}
	if err := FinishStep(ctx, testPool, step.ID, StepFailed, "boom"); err != nil {
		t.Fatalf("FinishStep: %v", err)
	}
	if err := Transition(ctx, testPool, job.ID, StatusRunning, StatusFailed, JobStatusFields{FailureReason: strPtr("boom")}); err != nil {
		t.Fatalf("transition to failed: %v", err)
	}
	job.Status = StatusFailed
	return job
}

func strPtr(s string) *string { return &s }

func TestRetryJob_TransitionsFailedToQueued(t *testing.T) {
	t.Parallel()
	job := seedFailedJobWithFailedStep(t)

	if err := RetryJob(context.Background(), testPool, job.ID); err != nil {
		t.Fatalf("RetryJob() error = %v", err)
	}

	got, err := GetJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != StatusQueued {
		t.Errorf("Status = %s, want QUEUED", got.Status)
	}
}

// TestRetryJob_NoFailedStepFails proves a job that failed before any
// step ran (e.g. a planner error) can't be retried — there's no
// partial progress to resume from.
func TestRetryJob_NoFailedStepFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job := seedJob(t) // PENDING_PLAN, no steps at all
	if err := Transition(ctx, testPool, job.ID, StatusPendingPlan, StatusPlanning, JobStatusFields{AcquireLease: &LeaseGrant{Owner: "w1", TTL: time.Minute}}); err != nil {
		t.Fatalf("transition to planning: %v", err)
	}
	if err := Transition(ctx, testPool, job.ID, StatusPlanning, StatusFailed, JobStatusFields{FailureReason: strPtr("planner error")}); err != nil {
		t.Fatalf("transition to failed: %v", err)
	}

	err := RetryJob(ctx, testPool, job.ID)
	if !errors.Is(err, ErrNoStepsToRetry) {
		t.Errorf("RetryJob() error = %v, want ErrNoStepsToRetry", err)
	}
}

func TestRetryJob_WrongStatusFails(t *testing.T) {
	t.Parallel()
	job := seedQueuedJob(t) // QUEUED, not FAILED

	var illegal *IllegalTransitionError
	err := RetryJob(context.Background(), testPool, job.ID)
	if !errors.As(err, &illegal) {
		t.Errorf("RetryJob() error = %v, want IllegalTransitionError", err)
	}
}
