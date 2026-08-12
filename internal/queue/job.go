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
	// SandboxID is the Runner's sandbox for this job, once one has been
	// created. Persisted so that a restarted worker resuming this job
	// reattaches to the same, still-running sandbox instead of creating a
	// fresh one — a fresh sandbox would silently lose whatever earlier
	// steps had already done inside it.
	SandboxID string
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
          run_after, created_at, started_at, finished_at, COALESCE(sandbox_id, '')`

// CreateJob inserts a new job in PENDING_PLAN, immediately claimable.
func CreateJob(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, prompt string) (*Job, error) {
	var j Job
	err := pool.QueryRow(ctx, createJobSQL, userID, prompt).Scan(
		&j.ID, &j.UserID, &j.Prompt, &j.Status, &j.FailureReason,
		&j.Attempt, &j.MaxAttempts, &j.LeaseOwner, &j.LeaseExpiresAt,
		&j.RunAfter, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.SandboxID,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: create job: %w", err)
	}
	return &j, nil
}

const createQueuedJobSQL = `
INSERT INTO jobs (user_id, prompt, status, started_at)
VALUES ($1, $2, 'QUEUED', NULL)
RETURNING id, user_id, prompt, status, COALESCE(failure_reason, ''),
          attempt, max_attempts, COALESCE(lease_owner, ''), lease_expires_at,
          run_after, created_at, started_at, finished_at, COALESCE(sandbox_id, '')`

// CreateQueuedJob inserts a new job already in QUEUED, skipping
// PENDING_PLAN and PLANNING entirely. Phase 1 has no planner to produce a
// plan for those states to represent, so a job's plan is decided before
// this is even called (right now: a fixed slice of steps in
// internal/executor) and there's nothing to wait on approval for.
func CreateQueuedJob(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, prompt string) (*Job, error) {
	var j Job
	err := pool.QueryRow(ctx, createQueuedJobSQL, userID, prompt).Scan(
		&j.ID, &j.UserID, &j.Prompt, &j.Status, &j.FailureReason,
		&j.Attempt, &j.MaxAttempts, &j.LeaseOwner, &j.LeaseExpiresAt,
		&j.RunAfter, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.SandboxID,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: create queued job: %w", err)
	}
	return &j, nil
}

const setJobSandboxIDSQL = `UPDATE jobs SET sandbox_id = $2 WHERE id = $1`

// SetJobSandboxID records the sandbox created for jobID, once. A separate
// statement from Transition on purpose — this isn't a status change, so
// I-1's single-writer rule for jobs.status doesn't apply to it, and
// tying it to a status transition would force one to happen just to
// persist this.
func SetJobSandboxID(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID, sandboxID string) error {
	if _, err := pool.Exec(ctx, setJobSandboxIDSQL, jobID, sandboxID); err != nil {
		return fmt.Errorf("queue: set sandbox id for job %s: %w", jobID, err)
	}
	return nil
}

const getJobSQL = `
SELECT id, user_id, prompt, status, COALESCE(failure_reason, ''),
       attempt, max_attempts, COALESCE(lease_owner, ''), lease_expires_at,
       run_after, created_at, started_at, finished_at, COALESCE(sandbox_id, '')
FROM jobs WHERE id = $1`

// getJob reads jobID's current row.
func getJob(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) (*Job, error) {
	return getJobVia(ctx, pool, jobID)
}

// GetJob reads jobID's current row, or ErrNotFound if it doesn't exist —
// the exported form of getJob, for callers outside this package (the API
// layer looking up a job to serve or to authorize an SSE subscription).
func GetJob(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) (*Job, error) {
	j, err := getJobVia(ctx, pool, jobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("queue: get job %s: %w", jobID, ErrNotFound)
		}
		return nil, err
	}
	return j, nil
}

const listJobsForUserSQL = `
SELECT id, user_id, prompt, status, COALESCE(failure_reason, ''),
       attempt, max_attempts, COALESCE(lease_owner, ''), lease_expires_at,
       run_after, created_at, started_at, finished_at, COALESCE(sandbox_id, '')
FROM jobs WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`

// ListJobsForUser returns userID's jobs, newest first, paginated.
func ListJobsForUser(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, limit, offset int) ([]*Job, error) {
	rows, err := pool.Query(ctx, listJobsForUserSQL, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("queue: list jobs for user %s: %w", userID, err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(
			&j.ID, &j.UserID, &j.Prompt, &j.Status, &j.FailureReason,
			&j.Attempt, &j.MaxAttempts, &j.LeaseOwner, &j.LeaseExpiresAt,
			&j.RunAfter, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.SandboxID,
		); err != nil {
			return nil, fmt.Errorf("queue: list jobs for user %s: scan: %w", userID, err)
		}
		jobs = append(jobs, &j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list jobs for user %s: %w", userID, err)
	}
	return jobs, nil
}

// getJobVia reads jobID's current row through q — a *pgxpool.Pool or a
// pgx.Tx, so callers inside a transaction (e.g. sweeper.deadLetterJob) see
// their own uncommitted writes.
func getJobVia(ctx context.Context, q querier, jobID uuid.UUID) (*Job, error) {
	var j Job
	err := q.QueryRow(ctx, getJobSQL, jobID).Scan(
		&j.ID, &j.UserID, &j.Prompt, &j.Status, &j.FailureReason,
		&j.Attempt, &j.MaxAttempts, &j.LeaseOwner, &j.LeaseExpiresAt,
		&j.RunAfter, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.SandboxID,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: get job %s: %w", jobID, err)
	}
	return &j, nil
}
