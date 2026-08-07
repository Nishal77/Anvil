package queue

import (
	"context"
	"errors"
	"testing"
)

func TestINV4_IllegalTransitionIsRejectedAndDoesNotWrite(t *testing.T) {
	t.Parallel()
	job := seedUnclaimableJob(t) // PENDING_PLAN, protected from Claim()

	err := Transition(context.Background(), testPool, job.ID, StatusPendingPlan, StatusRunning, JobStatusFields{})

	var illegal *IllegalTransitionError
	if !errors.As(err, &illegal) {
		t.Fatalf("Transition(PENDING_PLAN -> RUNNING) = %v, want IllegalTransitionError", err)
	}

	got, err := getJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("getJob() error: %v", err)
	}
	if got.Status != StatusPendingPlan {
		t.Errorf("status changed to %s despite the rejected transition", got.Status)
	}
}

func TestQueue_Transition_StaleFromIsRejected(t *testing.T) {
	t.Parallel()
	job := seedUnclaimableJob(t) // actually PENDING_PLAN, protected from Claim()

	// Claiming from AWAITING_APPROVAL is a legal edge in the graph, but
	// this row isn't actually in that state — the WHERE clause's
	// optimistic-concurrency check must catch the mismatch even though
	// the static graph allows the edge.
	err := Transition(context.Background(), testPool, job.ID, StatusAwaitingApproval, StatusQueued, JobStatusFields{})

	var illegal *IllegalTransitionError
	if !errors.As(err, &illegal) {
		t.Fatalf("Transition() with a stale `from` = %v, want IllegalTransitionError", err)
	}
}

func TestQueue_Transition_TerminalReleasesLeaseAndSetsFinishedAt(t *testing.T) {
	t.Parallel()
	job := seedClaimedJob(t, "terminal-test-owner")
	// This helper's job is PLANNING; move it via the queue's own legal
	// edge into AWAITING_APPROVAL first isn't needed — PLANNING -> FAILED
	// is directly legal and terminal, which is what this test checks.

	if err := Transition(context.Background(), testPool, job.ID, StatusPlanning, StatusFailed, JobStatusFields{}); err != nil {
		t.Fatalf("Transition(PLANNING -> FAILED) error: %v", err)
	}

	got, err := getJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("getJob() error: %v", err)
	}
	if got.LeaseOwner != "" || got.LeaseExpiresAt != nil {
		t.Errorf("terminal transition left a lease: owner=%q expires=%v", got.LeaseOwner, got.LeaseExpiresAt)
	}
	if got.FinishedAt == nil {
		t.Error("terminal transition did not set finished_at")
	}
}

func TestQueue_Transition_CannotReleaseLeaseIntoLeaseRequiringState(t *testing.T) {
	t.Parallel()
	job := seedUnclaimableJob(t)

	err := Transition(context.Background(), testPool, job.ID, StatusPendingPlan, StatusPlanning, JobStatusFields{
		ReleaseLease: true,
	})
	if err == nil {
		t.Fatal("Transition() with ReleaseLease into PLANNING succeeded, want an error (INV-1)")
	}
}
