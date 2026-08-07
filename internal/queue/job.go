package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status mirrors the job_status enum defined in PRD §13.1.
type Status string

const (
	StatusPendingPlan      Status = "PENDING_PLAN"
	StatusPlanning         Status = "PLANNING"
	StatusAwaitingApproval Status = "AWAITING_APPROVAL"
	StatusQueued           Status = "QUEUED"
	StatusRunning          Status = "RUNNING"
	StatusDeploying        Status = "DEPLOYING"
	StatusSucceeded        Status = "SUCCEEDED"
	StatusFailed           Status = "FAILED"
	StatusCancelled        Status = "CANCELLED"
)

// Job is a row of the jobs table (PRD §10).
type Job struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	Prompt         string
	Status         Status
	FailureReason  string
	Attempt        int
	MaxAttempts    int
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	RunAfter       time.Time
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
}

// JobStatusFields carries the optional column writes that accompany a
// status transition, so Transition stays one statement (PRD §13.1).
type JobStatusFields struct {
	FailureReason *string
	ReleaseLease  bool
	SetStartedAt  bool
	SetFinishedAt bool
	// AcquireLease, if non-nil, grants a fresh lease and increments
	// attempt as part of this same UPDATE — set only by Claim, whose
	// SELECT ... FOR UPDATE SKIP LOCKED and status write must be one
	// atomic statement with the lease grant (PRD §14.2(a)).
	AcquireLease *LeaseGrant
	// RunAfter, if non-nil, sets the backoff gate (FR-014) — set by the
	// sweeper on reclaim.
	RunAfter *time.Time
}

const createJobSQL = `
INSERT INTO jobs (user_id, prompt)
VALUES ($1, $2)
RETURNING id, user_id, prompt, status, COALESCE(failure_reason, ''),
          attempt, max_attempts, COALESCE(lease_owner, ''), lease_expires_at,
          run_after, created_at, started_at, finished_at`

// CreateJob inserts a new job in PENDING_PLAN, immediately claimable.
func CreateJob(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, prompt string) (*Job, error) {
	var j Job
	err := pool.QueryRow(ctx, createJobSQL, userID, prompt).Scan(
		&j.ID, &j.UserID, &j.Prompt, &j.Status, &j.FailureReason,
		&j.Attempt, &j.MaxAttempts, &j.LeaseOwner, &j.LeaseExpiresAt,
		&j.RunAfter, &j.CreatedAt, &j.StartedAt, &j.FinishedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: create job: %w", err)
	}
	return &j, nil
}

const getJobSQL = `
SELECT id, user_id, prompt, status, COALESCE(failure_reason, ''),
       attempt, max_attempts, COALESCE(lease_owner, ''), lease_expires_at,
       run_after, created_at, started_at, finished_at
FROM jobs WHERE id = $1`

// getJob reads jobID's current row.
func getJob(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) (*Job, error) {
	return getJobVia(ctx, pool, jobID)
}

// getJobVia reads jobID's current row through q — a *pgxpool.Pool or a
// pgx.Tx, so callers inside a transaction (e.g. sweeper.deadLetterJob) see
// their own uncommitted writes.
func getJobVia(ctx context.Context, q querier, jobID uuid.UUID) (*Job, error) {
	var j Job
	err := q.QueryRow(ctx, getJobSQL, jobID).Scan(
		&j.ID, &j.UserID, &j.Prompt, &j.Status, &j.FailureReason,
		&j.Attempt, &j.MaxAttempts, &j.LeaseOwner, &j.LeaseExpiresAt,
		&j.RunAfter, &j.CreatedAt, &j.StartedAt, &j.FinishedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: get job %s: %w", jobID, err)
	}
	return &j, nil
}
