package queue

import (
	"context"
	"testing"
)

// TestFR020_PlanPersistedInSingleTransaction proves SavePlan's step
// rows and job status change land together: a plan with the seeded
// job's status still PLANNING beforehand, once SavePlan returns, must
// have every step row present — never a job that believes it has a
// plan with zero steps to show for it.
func TestFR020_PlanPersistedInSingleTransaction(t *testing.T) {
	t.Parallel()
	job := seedClaimedJob(t, "planner-owner")

	steps := []PlannedStep{
		{Title: "one", Description: "d1", Acceptance: "a1"},
		{Title: "two", Description: "d2", Acceptance: "a2", Optional: true},
	}
	if err := SavePlan(context.Background(), testPool, job.ID, "summary", []string{"risk one"}, steps, false); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	got, err := getJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("getJob: %v", err)
	}
	if got.Status != StatusAwaitingApproval {
		t.Errorf("Status = %s, want AWAITING_APPROVAL", got.Status)
	}
	if got.PlanSummary != "summary" {
		t.Errorf("PlanSummary = %q, want %q", got.PlanSummary, "summary")
	}
	if got.LeaseOwner != "" {
		t.Errorf("LeaseOwner = %q, want cleared on exit from PLANNING", got.LeaseOwner)
	}

	rows, err := ListSteps(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(steps) = %d, want 2", len(rows))
	}
	if rows[1].Optional != true || rows[1].Acceptance != "a2" {
		t.Errorf("step 1 = %+v, want Optional=true Acceptance=a2", rows[1])
	}
}

// TestFR020_AutoApproveSkipsToQueued proves a job with auto_approve
// lands directly in QUEUED, bypassing AWAITING_APPROVAL entirely.
func TestFR020_AutoApproveSkipsToQueued(t *testing.T) {
	t.Parallel()
	job := seedClaimedJob(t, "planner-owner-auto")

	steps := []PlannedStep{{Title: "one", Description: "d1", Acceptance: "a1"}}
	if err := SavePlan(context.Background(), testPool, job.ID, "summary", nil, steps, true); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	got, err := getJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("getJob: %v", err)
	}
	if got.Status != StatusQueued {
		t.Errorf("Status = %s, want QUEUED", got.Status)
	}
}

// TestUS02_ApproveTransitionsToQueued proves ApproveJob moves a plan
// out of AWAITING_APPROVAL, and TestINV4_CannotExecuteWhileAwaitingApproval
// proves the state machine — not the frontend — is what blocks a job
// still awaiting approval from ever reaching RUNNING directly.
func TestUS02_ApproveTransitionsToQueued(t *testing.T) {
	t.Parallel()
	job := seedClaimedJob(t, "approve-owner")
	if err := SavePlan(context.Background(), testPool, job.ID, "s", nil, []PlannedStep{{Title: "t", Description: "d", Acceptance: "a"}}, false); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	if err := ApproveJob(context.Background(), testPool, job.ID); err != nil {
		t.Fatalf("ApproveJob: %v", err)
	}

	got, err := getJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("getJob: %v", err)
	}
	if got.Status != StatusQueued {
		t.Errorf("Status = %s, want QUEUED", got.Status)
	}
}

func TestINV4_CannotExecuteWhileAwaitingApproval(t *testing.T) {
	t.Parallel()
	job := seedClaimedJob(t, "inv4-owner")
	if err := SavePlan(context.Background(), testPool, job.ID, "s", nil, []PlannedStep{{Title: "t", Description: "d", Acceptance: "a"}}, false); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	// A job AWAITING_APPROVAL is not claimable: Claim only selects
	// PENDING_PLAN and QUEUED (claimCandidateSQL). It cannot reach
	// RUNNING by any transition either — AWAITING_APPROVAL's only
	// legal edges are to QUEUED (via ApproveJob) or CANCELLED.
	err := Transition(context.Background(), testPool, job.ID, StatusAwaitingApproval, StatusRunning, JobStatusFields{})
	if err == nil {
		t.Fatal("Transition AWAITING_APPROVAL -> RUNNING succeeded, want IllegalTransitionError")
	}
}
