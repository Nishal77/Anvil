package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFR011_HeartbeatExtendsLease(t *testing.T) {
	t.Parallel()
	job := seedClaimedJob(t, "heartbeat-owner")

	before, err := getJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("getJob() error: %v", err)
	}

	if err := Heartbeat(context.Background(), testPool, job.ID, "heartbeat-owner", 5*time.Minute); err != nil {
		t.Fatalf("Heartbeat() error: %v", err)
	}

	after, err := getJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("getJob() error: %v", err)
	}
	if !after.LeaseExpiresAt.After(*before.LeaseExpiresAt) {
		t.Errorf("lease_expires_at did not move forward: before=%v after=%v", before.LeaseExpiresAt, after.LeaseExpiresAt)
	}
}

func TestFR011_HeartbeatByNonOwnerReturnsErrLeaseLost(t *testing.T) {
	t.Parallel()
	job := seedClaimedJob(t, "real-owner")

	err := Heartbeat(context.Background(), testPool, job.ID, "impostor", time.Minute)
	if !errors.Is(err, ErrLeaseLost) {
		t.Errorf("Heartbeat() by non-owner = %v, want ErrLeaseLost", err)
	}
}

// seedClaimedJob seeds a job already in PLANNING with a live lease owned
// by workerID, set directly rather than via Claim — Claim picks whichever
// job is claimable, which is nondeterministic under t.Parallel() against a
// shared database with sibling tests seeding their own claimable jobs.
// Tests here care about lease behavior given a lease exists, not about
// Claim's selection logic (claim_test.go covers that).
func seedClaimedJob(t *testing.T, workerID string) *Job {
	t.Helper()
	job := seedJob(t)
	ctx := context.Background()
	_, err := testPool.Exec(ctx,
		`UPDATE jobs SET status = 'PLANNING', lease_owner = $2,
		 lease_expires_at = now() + interval '1 minute', attempt = 1
		 WHERE id = $1`,
		job.ID, workerID)
	if err != nil {
		t.Fatalf("seedClaimedJob: %v", err)
	}
	got, err := getJob(ctx, testPool, job.ID)
	if err != nil {
		t.Fatalf("seedClaimedJob: getJob: %v", err)
	}
	return got
}
