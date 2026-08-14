package queue

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeClock lets tests control time deterministically (CLAUDE.md T4 — no
// test sleeps). Now is mutable; After fires as soon as it's called, since
// these tests only need sweep() called directly, not a running ticker
// loop.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

func TestFR012_ExpiredLeaseIsReclaimedAndAttemptIncrements(t *testing.T) {
	t.Parallel()
	job := seedClaimedJob(t, "expiring-owner")
	expireLeaseNow(t, job.ID)

	// sweep() drains every expired row in the table, not just this test's
	// job — under t.Parallel() a sibling test's own sweep() call can
	// legitimately reclaim this row, possibly in a transaction still
	// committing when this call returns (READ COMMITTED: a fresh getJob()
	// won't see it until commit lands). The outcome that matters is the
	// row's eventual state, not which sweep() invocation performed the
	// write or whether this test's own read landed before that commit.
	clk := &fakeClock{now: time.Now()}
	if _, _, _, _, err := sweep(context.Background(), testPool, clk); err != nil {
		t.Fatalf("sweep() error: %v", err)
	}

	var got *Job
	waitFor(t, 2*time.Second, func() bool {
		var err error
		got, err = getJob(context.Background(), testPool, job.ID)
		return err == nil && got.Status != StatusPlanning
	}, func() {
		t.Fatal("job was never reclaimed out of PLANNING")
	})
	if got.Status != StatusPendingPlan {
		t.Errorf("status = %s, want PENDING_PLAN (reclaimed from PLANNING)", got.Status)
	}
	if got.LeaseOwner != "" {
		t.Errorf("LeaseOwner = %q, want cleared", got.LeaseOwner)
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1 (claim incremented it; reclaim does not increment again)", got.Attempt)
	}
}

func TestFR013_MaxAttemptsExceededDeadLettersWithSnapshot(t *testing.T) {
	t.Parallel()
	job := seedClaimedJob(t, "exhausted-owner")
	setAttempt(t, job.ID, 3) // == default max_attempts
	expireLeaseNow(t, job.ID)

	// Same reasoning as TestFR012: don't assert on this call's own return
	// counts or read immediately — a sibling's concurrent sweep() may
	// have already handled this row in a transaction still committing.
	clk := &fakeClock{now: time.Now()}
	if _, _, _, _, err := sweep(context.Background(), testPool, clk); err != nil {
		t.Fatalf("sweep() error: %v", err)
	}

	var got *Job
	waitFor(t, 2*time.Second, func() bool {
		var err error
		got, err = getJob(context.Background(), testPool, job.ID)
		return err == nil && got.Status == StatusFailed
	}, func() {
		t.Fatal("job was never dead-lettered")
	})

	var snapshotJSON []byte
	var lastError string
	err := testPool.QueryRow(context.Background(),
		`SELECT snapshot, last_error FROM dead_letter_jobs WHERE job_id = $1`, job.ID,
	).Scan(&snapshotJSON, &lastError)
	if err != nil {
		t.Fatalf("dead_letter_jobs row missing: %v", err)
	}
	if len(snapshotJSON) == 0 {
		t.Error("dead_letter_jobs.snapshot is empty")
	}
	if lastError == "" {
		t.Error("dead_letter_jobs.last_error is empty")
	}
}

// TestUS04_WedgedWorkerReachesCancelledViaLeaseExpiry proves PRD §13.3
// step 5: a job whose worker never acknowledges a cancel request (the
// worker is wedged, not just slow) still reaches CANCELLED once its
// lease expires — cancellation does not depend on a live, cooperating
// worker to complete.
func TestUS04_WedgedWorkerReachesCancelledViaLeaseExpiry(t *testing.T) {
	t.Parallel()
	job := seedClaimedJob(t, "wedged-owner")
	if err := RequestCancel(context.Background(), testPool, job.ID); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	expireLeaseNow(t, job.ID)

	clk := &fakeClock{now: time.Now()}
	if _, _, _, _, err := sweep(context.Background(), testPool, clk); err != nil {
		t.Fatalf("sweep() error: %v", err)
	}

	var got *Job
	waitFor(t, 2*time.Second, func() bool {
		var err error
		got, err = getJob(context.Background(), testPool, job.ID)
		return err == nil && got.Status == StatusCancelled
	}, func() {
		t.Fatal("wedged job was never force-cancelled by the sweeper")
	})
	if got.LeaseOwner != "" {
		t.Errorf("LeaseOwner = %q, want cleared", got.LeaseOwner)
	}
}

func expireLeaseNow(t *testing.T, jobID uuid.UUID) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, jobID)
	if err != nil {
		t.Fatalf("expireLeaseNow: %v", err)
	}
}

func setAttempt(t *testing.T, jobID uuid.UUID, attempt int) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`UPDATE jobs SET attempt = $2 WHERE id = $1`, jobID, attempt)
	if err != nil {
		t.Fatalf("setAttempt: %v", err)
	}
}
