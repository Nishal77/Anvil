package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// reclaimTarget is the status an expired lease reclaims a job back to —
// the reverse of claimTargetStatus. DEPLOYING has no entry: the job
// lifecycle (PRD §13.1) has no reclaim edge out of DEPLOYING, so an
// expired-lease DEPLOYING job is always dead-lettered, never reclaimed.
var reclaimTarget = map[Status]Status{
	StatusPlanning: StatusPendingPlan,
	StatusRunning:  StatusQueued,
}

// expiredLeaseCandidateSQL finds and locks one lease-holding job whose
// lease has expired. lease_expires_at < now() is checked here, in the
// WHERE clause — never read in Go and compared, which would race a
// concurrent heartbeat extending the same row between the read and a
// later write. One row, not a batch: the lock this takes must be held for
// the full decide-and-write, which only a single transaction guarantees —
// a batch SELECT's lock does not survive past that one statement.
//
// cancel_requested_at and sandbox_id are read alongside status: a job
// whose worker never acknowledged a cancel request within the lease TTL
// is "wedged" (PRD §13.3 step 5) and force-cancels here regardless of
// attempt < max_attempts — cancellation always wins over retry.
const expiredLeaseCandidateSQL = `
SELECT id, status, attempt, max_attempts, cancel_requested_at IS NOT NULL, COALESCE(sandbox_id, '')
FROM jobs
WHERE status IN ('PLANNING', 'RUNNING', 'DEPLOYING')
  AND lease_expires_at < now()
ORDER BY lease_expires_at
FOR UPDATE SKIP LOCKED
LIMIT 1`

// sweepOutcome is what sweepOnce did to the one row it handled.
type sweepOutcome int

const (
	sweepReclaimed sweepOutcome = iota
	sweepDeadLettered
	sweepCancelled
)

// sweep reclaims jobs whose lease expired and attempt < max_attempts
// (FR-012), force-cancels wedged jobs with a pending cancel request
// (PRD §13.3 step 5), and dead-letters the rest (FR-013, FR-015) — one
// row per transaction until no more candidates remain. Concurrent
// sweepers (or a sweeper racing a worker's own graceful-shutdown
// reclaim) never contend for the same row: SKIP LOCKED means each just
// moves on to the next one. cancelledSandboxIDs is the set of sandboxes
// a force-cancelled job left running, which sweep cannot destroy itself
// (queue never touches the sandbox client — CLAUDE.md PK5) but the
// caller must, or a wedged worker's container leaks forever.
func sweep(ctx context.Context, pool *pgxpool.Pool, clk Clock) (reclaimed, deadLettered, cancelled int, cancelledSandboxIDs []string, err error) {
	for {
		handled, outcome, sandboxID, err := sweepOnce(ctx, pool, clk)
		if err != nil {
			return reclaimed, deadLettered, cancelled, cancelledSandboxIDs, err
		}
		if !handled {
			return reclaimed, deadLettered, cancelled, cancelledSandboxIDs, nil
		}
		switch outcome {
		case sweepReclaimed:
			reclaimed++
		case sweepCancelled:
			cancelled++
			if sandboxID != "" {
				cancelledSandboxIDs = append(cancelledSandboxIDs, sandboxID)
			}
		case sweepDeadLettered:
			deadLettered++
		}
	}
}

// expiredLeaseCandidate is one row of expiredLeaseCandidateSQL.
type expiredLeaseCandidate struct {
	id                   uuid.UUID
	status               Status
	attempt, maxAttempts int
	cancelRequested      bool
	sandboxID            string
}

// sweepOnce claims and handles exactly one expired-lease row. handled is
// false when there was no candidate left.
func sweepOnce(ctx context.Context, pool *pgxpool.Pool, clk Clock) (handled bool, outcome sweepOutcome, sandboxID string, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, 0, "", fmt.Errorf("queue: sweep: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var c expiredLeaseCandidate
	err = tx.QueryRow(ctx, expiredLeaseCandidateSQL).Scan(&c.id, &c.status, &c.attempt, &c.maxAttempts, &c.cancelRequested, &c.sandboxID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, "", nil
		}
		return false, 0, "", fmt.Errorf("queue: sweep: select candidate: %w", err)
	}

	switch {
	case c.cancelRequested:
		return sweepForceCancel(ctx, tx, c)
	case canReclaim(c):
		return sweepReclaim(ctx, tx, clk, c)
	default:
		return sweepDeadLetter(ctx, tx, c)
	}
}

func canReclaim(c expiredLeaseCandidate) bool {
	_, ok := reclaimTarget[c.status]
	return ok && c.attempt < c.maxAttempts
}

func sweepForceCancel(ctx context.Context, tx pgx.Tx, c expiredLeaseCandidate) (bool, sweepOutcome, string, error) {
	if err := Transition(ctx, tx, c.id, c.status, StatusCancelled, JobStatusFields{ReleaseLease: true}); err != nil {
		return false, 0, "", fmt.Errorf("queue: sweep: force-cancel wedged job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, 0, "", fmt.Errorf("queue: sweep: commit force-cancel: %w", err)
	}
	return true, sweepCancelled, c.sandboxID, nil
}

func sweepReclaim(ctx context.Context, tx pgx.Tx, clk Clock, c expiredLeaseCandidate) (bool, sweepOutcome, string, error) {
	runAfter := NextRunAfter(clk, c.attempt)
	if err := reclaimJobAt(ctx, tx, c.id, c.status, reclaimTarget[c.status], runAfter); err != nil {
		return false, 0, "", fmt.Errorf("queue: sweep: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, 0, "", fmt.Errorf("queue: sweep: commit reclaim: %w", err)
	}
	return true, sweepReclaimed, "", nil
}

func sweepDeadLetter(ctx context.Context, tx pgx.Tx, c expiredLeaseCandidate) (bool, sweepOutcome, string, error) {
	if err := deadLetterJob(ctx, tx, c.id, c.status); err != nil {
		return false, 0, "", fmt.Errorf("queue: sweep: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, 0, "", fmt.Errorf("queue: sweep: commit dead-letter: %w", err)
	}
	return true, sweepDeadLettered, "", nil
}

// reclaimJobAt transitions jobID from its lease-holding status back to its
// claimable predecessor, releasing the lease and setting run_after. Used
// by the sweeper (runAfter computed via backoff) and by a worker's
// graceful shutdown path (runAfter = now, immediate reclaim rather than
// stranding the job for the sweep interval) — see dispatcher.go.
func reclaimJobAt(ctx context.Context, q querier, jobID uuid.UUID, from, to Status, runAfter time.Time) error {
	if err := Transition(ctx, q, jobID, from, to, JobStatusFields{
		ReleaseLease: true,
		RunAfter:     &runAfter,
	}); err != nil {
		return fmt.Errorf("reclaim job %s: %w", jobID, err)
	}
	return nil
}

const insertDeadLetterSQL = `
INSERT INTO dead_letter_jobs (job_id, attempts, last_error, snapshot)
VALUES ($1, $2, $3, $4)
ON CONFLICT (job_id) DO NOTHING`

// deadLetterJob reads jobID's current row for the snapshot, transitions it
// to FAILED, and records it in dead_letter_jobs — all via q, so the
// caller's transaction covers the whole operation.
func deadLetterJob(ctx context.Context, q querier, jobID uuid.UUID, from Status) error {
	job, err := getJobVia(ctx, q, jobID)
	if err != nil {
		return fmt.Errorf("dead-letter job %s: %w", jobID, err)
	}
	snapshot, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("dead-letter job %s: marshal snapshot: %w", jobID, err)
	}

	reason := "max_attempts_exceeded"
	if err := Transition(ctx, q, jobID, from, StatusFailed, JobStatusFields{
		FailureReason: &reason,
	}); err != nil {
		return fmt.Errorf("dead-letter job %s: %w", jobID, err)
	}

	if _, err := q.Exec(ctx, insertDeadLetterSQL, jobID, job.Attempt, reason, snapshot); err != nil {
		return fmt.Errorf("dead-letter job %s: insert snapshot: %w", jobID, err)
	}
	return nil
}
