package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// claimCandidateSQL selects one claimable job and locks it, per PRD
// §14.2(a). Subquery form, not a CTE with a separate UPDATE — a CTE
// (WITH ... AS (SELECT ... FOR UPDATE SKIP LOCKED)) takes its lock at a
// different point in the plan and does not give the same skip-locked
// guarantee under all planner choices. FOR UPDATE SKIP LOCKED is what
// makes N workers safe without any external coordination: a row locked by
// another worker's transaction is skipped rather than waited on, so
// workers never contend and never deadlock.
const claimCandidateSQL = `
SELECT id, status FROM jobs
WHERE status IN ('PENDING_PLAN', 'QUEUED')
  AND run_after <= now()
  AND attempt < max_attempts
ORDER BY created_at
FOR UPDATE SKIP LOCKED
LIMIT 1`

// claimTargetStatus is the state a claimed job moves to, keyed by the
// state it was claimed from (PRD §14.2(a)'s CASE expression).
var claimTargetStatus = map[Status]Status{
	StatusPendingPlan: StatusPlanning,
	StatusQueued:      StatusRunning,
}

// Claim finds and atomically claims one claimable job for workerID. The
// SELECT ... FOR UPDATE SKIP LOCKED locks the row; Transition then
// performs the write, inside the same transaction — so the lock taken by
// the SELECT covers the UPDATE and no other claimer can race it. Returns
// ErrNoWork if nothing was claimable.
func Claim(ctx context.Context, pool *pgxpool.Pool, workerID string, leaseTTL time.Duration) (*Job, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("queue: claim: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id uuid.UUID
	var from Status
	err = tx.QueryRow(ctx, claimCandidateSQL).Scan(&id, &from)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoWork
		}
		return nil, fmt.Errorf("queue: claim: select candidate: %w", err)
	}

	to, ok := claimTargetStatus[from]
	if !ok {
		return nil, fmt.Errorf("queue: claim: job %s has unclaimable status %s", id, from)
	}

	if err := Transition(ctx, tx, id, from, to, JobStatusFields{
		AcquireLease: &LeaseGrant{Owner: workerID, TTL: leaseTTL},
	}); err != nil {
		return nil, fmt.Errorf("queue: claim: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("queue: claim: commit: %w", err)
	}

	return getJob(ctx, pool, id)
}
