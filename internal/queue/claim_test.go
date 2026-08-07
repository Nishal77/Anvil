package queue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Not t.Parallel(): this test needs its seeded job to be claimed only by
// its own 8 workers. Claim() has no per-test scoping — it claims whichever
// job is oldest-claimable in the whole shared table — so running this
// concurrently with any other test that also calls Claim() risks an
// unrelated test's worker stealing this job first. Go never runs
// non-parallel tests concurrently with each other, which is what actually
// gives this test its isolation.
func TestFR010_EightWorkersClaimSameJob_OnlyOneSucceeds(t *testing.T) {
	job := seedJob(t)

	const workers = 8
	var (
		start   = make(chan struct{})
		claimed atomic.Int32
		wg      sync.WaitGroup
	)

	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got, err := Claim(context.Background(), testPool, workerIDFor(t, i), time.Minute)
			if err != nil {
				if !errors.Is(err, ErrNoWork) {
					t.Errorf("unexpected claim error: %v", err)
				}
				return
			}
			if got.ID == job.ID {
				claimed.Add(1)
			}
		}(i)
	}

	close(start)
	wg.Wait()

	if got := claimed.Load(); got != 1 {
		t.Fatalf("job claimed %d times, want exactly 1 (FOR UPDATE SKIP LOCKED must serialize)", got)
	}
}

// TestFR010_ClaimSkipsJobsNotYetRunnable proves the run_after filter,
// without asserting global "nothing claimable" — that's unprovable here:
// this test runs under t.Parallel() against a shared database, and sibling
// tests are seeding claimable jobs concurrently. What's provable in that
// environment is that a specific not-yet-runnable job is never the one
// returned.
func TestFR010_ClaimSkipsJobsNotYetRunnable(t *testing.T) {
	t.Parallel()
	job := seedJob(t)
	_, err := testPool.Exec(context.Background(),
		`UPDATE jobs SET run_after = now() + interval '1 hour' WHERE id = $1`, job.ID)
	if err != nil {
		t.Fatalf("set future run_after: %v", err)
	}

	for range 5 {
		got, err := Claim(context.Background(), testPool, "run-after-probe-worker", time.Minute)
		if err != nil {
			if errors.Is(err, ErrNoWork) {
				return
			}
			t.Fatalf("Claim() error: %v", err)
		}
		if got.ID == job.ID {
			t.Fatal("Claim() returned a job whose run_after is an hour in the future")
		}
	}
}

// Not t.Parallel() — see TestFR010_EightWorkersClaimSameJob_OnlyOneSucceeds:
// this test needs Claim() to return exactly the job it just seeded.
func TestFR010_ClaimSetsAttemptAndLease(t *testing.T) {
	job := seedJob(t)

	got, err := Claim(context.Background(), testPool, "attempt-probe-worker", time.Minute)
	if err != nil {
		t.Fatalf("Claim() error: %v", err)
	}
	if got.ID != job.ID {
		t.Fatalf("Claim() returned job %s, want the freshly seeded job %s", got.ID, job.ID)
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1 (increments on claim, not on failure)", got.Attempt)
	}
	if got.Status != StatusPlanning {
		t.Errorf("Status = %s, want PLANNING", got.Status)
	}
	if got.LeaseOwner != "attempt-probe-worker" {
		t.Errorf("LeaseOwner = %q, want attempt-probe-worker", got.LeaseOwner)
	}
	if got.LeaseExpiresAt == nil {
		t.Error("LeaseExpiresAt is nil, want a lease grant")
	}
}

func workerIDFor(t *testing.T, i int) string {
	t.Helper()
	return t.Name() + "-worker-" + string(rune('a'+i))
}
