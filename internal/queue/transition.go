package queue

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// querier is the subset of pgx satisfied by both *pgxpool.Pool and pgx.Tx —
// Transition runs standalone against the pool, or as part of a larger
// transaction (Claim's select-then-write). Declared here, at the
// consumer, per CODE-STANDARDS §3.1.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LeaseGrant is the lease acquired atomically with a transition into a
// lease-requiring state (PLANNING, RUNNING, DEPLOYING). Set on
// JobStatusFields.AcquireLease by Claim; nil for every other transition.
type LeaseGrant struct {
	Owner string
	TTL   time.Duration
}

// allowedTransitions is the job lifecycle from PRD §13.1, encoded as a
// from -> to adjacency map. This is the complete graph, not just the
// subset Week 2 exercises: a partial graph would itself violate INV-4,
// which requires every illegal edge to be rejected, not just the ones a
// test happens to try.
var allowedTransitions = map[Status]map[Status]bool{
	// Every non-terminal status also reaches CANCELLED (PRD §13.3): a
	// job can be cancelled before planning starts, while planning is in
	// flight, while queued waiting for a worker, or while awaiting
	// approval — not only while RUNNING. Week 2 only exercised the
	// RUNNING edge; the others are Week 7's completion of this graph,
	// not new behavior for RUNNING itself.
	StatusPendingPlan: {StatusPlanning: true, StatusCancelled: true},
	StatusQueued:      {StatusRunning: true, StatusCancelled: true},
	// PLANNING -> QUEUED is the auto-approve path (options.auto_approve,
	// PRD §11): SavePlan skips AWAITING_APPROVAL entirely rather than
	// routing through it and immediately approving, which would leave a
	// visible but meaningless AWAITING_APPROVAL moment in the event log.
	StatusPlanning: {StatusPendingPlan: true, StatusAwaitingApproval: true, StatusQueued: true, StatusFailed: true, StatusCancelled: true},
	// RUNNING -> SUCCEEDED is not in the PRD §13.1 diagram directly, but is
	// the real path for a job submitted with options.deploy = false
	// (PRD §11.3) — such a job never enters DEPLOYING.
	StatusRunning:          {StatusQueued: true, StatusDeploying: true, StatusSucceeded: true, StatusFailed: true, StatusCancelled: true},
	StatusAwaitingApproval: {StatusQueued: true, StatusCancelled: true},
	StatusDeploying:        {StatusSucceeded: true, StatusFailed: true},
}

func isTerminal(s Status) bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCancelled
}

// requiresLease reports whether a job in status s must hold a live lease
// (INV-1: PLANNING | RUNNING | DEPLOYING).
func requiresLease(s Status) bool {
	return s == StatusPlanning || s == StatusRunning || s == StatusDeploying
}

// Transition performs THE single guarded status change for jobID: checks
// INV-1 through INV-5 (PRD §13.1) before writing, rejects any edge the job
// lifecycle doesn't permit with IllegalTransitionError, and is the only
// place in the codebase that issues `UPDATE jobs SET status`
// (CLAUDE.md I-1 — enforced by scripts/check-invariants.sh).
//
// INV-1 and INV-2 are enforced by construction, not by trusting the
// caller: a transition into a lease-requiring state cannot also release
// the lease, and every transition into a terminal state releases the
// lease and stamps finished_at unconditionally, regardless of what
// fields requested.
func Transition(ctx context.Context, q querier, jobID uuid.UUID, from, to Status, fields JobStatusFields) error {
	if !allowedTransitions[from][to] {
		return &IllegalTransitionError{JobID: jobID, From: from, To: to}
	}
	if requiresLease(to) && fields.ReleaseLease {
		return fmt.Errorf("queue: transition job %s to %s: cannot release the lease while entering a lease-requiring state", jobID, to)
	}

	if isTerminal(to) {
		// Enforced by construction, not by trusting the caller (INV-2).
		fields.ReleaseLease = true
		fields.SetFinishedAt = true
	}

	sql, args := buildTransitionSQL(jobID, from, to, fields)
	tag, err := q.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("queue: transition job %s %s->%s: %w", jobID, from, to, err)
	}
	if tag.RowsAffected() == 0 {
		// The row wasn't in `from` (already moved, or never existed) —
		// from the database's current-state perspective this is exactly
		// the same failure as a statically illegal edge.
		return &IllegalTransitionError{JobID: jobID, From: from, To: to}
	}
	return nil
}

// buildTransitionSQL assembles the single UPDATE statement for a
// transition. Column inclusion is conditional on typed Go fields, never on
// concatenated data — every value is still a placeholder argument
// (CODE-STANDARDS D1).
func buildTransitionSQL(jobID uuid.UUID, from, to Status, fields JobStatusFields) (string, []any) {
	var sb strings.Builder
	args := []any{jobID, string(from), string(to)}
	sb.WriteString("UPDATE jobs SET status = $3, updated_at = now()")

	if fields.FailureReason != nil {
		args = append(args, *fields.FailureReason)
		fmt.Fprintf(&sb, ", failure_reason = $%d", len(args))
	}
	if fields.SetStartedAt {
		sb.WriteString(", started_at = COALESCE(started_at, now())")
	}
	if fields.SetFinishedAt {
		sb.WriteString(", finished_at = now()")
	}
	if fields.ReleaseLease {
		sb.WriteString(", lease_owner = NULL, lease_expires_at = NULL")
	}
	if fields.RunAfter != nil {
		args = append(args, *fields.RunAfter)
		fmt.Fprintf(&sb, ", run_after = $%d", len(args))
	}
	if fields.AcquireLease != nil {
		args = append(args, fields.AcquireLease.Owner, fields.AcquireLease.TTL)
		ownerIdx, ttlIdx := len(args)-1, len(args)
		// attempt increments on claim, not on failure — a crashed worker
		// must burn an attempt or a poison-pill job retries forever
		// (PRD §14.2(f)).
		fmt.Fprintf(&sb, ", lease_owner = $%d, lease_expires_at = now() + $%d::interval, attempt = attempt + 1, started_at = COALESCE(started_at, now())", ownerIdx, ttlIdx)
	}

	sb.WriteString(" WHERE id = $1 AND status = $2")
	return sb.String(), args
}
