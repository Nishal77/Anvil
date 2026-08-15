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
	// PlanSummary and PlanRisks are the planner's output (PRD §12.1),
	// set by SavePlan alongside the step rows it inserts.
	PlanSummary string
	PlanRisks   json.RawMessage
	// AutoApprove, set at submission (PRD §11's options.auto_approve),
	// tells SavePlan whether to land the plan in QUEUED directly or
	// AWAITING_APPROVAL.
	AutoApprove bool
	// CancelRequestedAt is non-nil once POST /cancel has been called
	// (PRD §13.3 step 1). The executor polls this between every turn.
	CancelRequestedAt *time.Time
	// ArtifactKey is the workspace archive's object key in artifact
	// storage, set once the upload completes on a terminal status
	// (SUCCEEDED, FAILED, or CANCELLED — ADR-012: failure preserves
	// the artifact). Empty until then.
	ArtifactKey string
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

// jobColumns is the column list shared by every query that reads a full
// Job row — one place to add a column instead of four (Create*, Get,
// List), so they can't drift apart from each other.
const jobColumns = `id, user_id, prompt, status, COALESCE(failure_reason, ''),
	attempt, max_attempts, COALESCE(lease_owner, ''), lease_expires_at,
	run_after, created_at, started_at, finished_at, COALESCE(sandbox_id, ''),
	COALESCE(plan_summary, ''), plan_risks, auto_approve, cancel_requested_at,
	COALESCE(artifact_key, '')`

// scanJob reads one jobColumns-shaped row into a Job.
func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	err := row.Scan(
		&j.ID, &j.UserID, &j.Prompt, &j.Status, &j.FailureReason,
		&j.Attempt, &j.MaxAttempts, &j.LeaseOwner, &j.LeaseExpiresAt,
		&j.RunAfter, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.SandboxID,
		&j.PlanSummary, &j.PlanRisks, &j.AutoApprove, &j.CancelRequestedAt,
		&j.ArtifactKey,
	)
	if err != nil {
		return nil, fmt.Errorf("queue: scan job: %w", err)
	}
	return &j, nil
}

// const string concatenation (both operands are untyped constants) —
// not a package-level var (CLAUDE.md §5.2).
const createJobSQL = `
INSERT INTO jobs (user_id, prompt, auto_approve)
VALUES ($1, $2, $3)
RETURNING ` + jobColumns

// CreateJob inserts a new job in PENDING_PLAN, immediately claimable by
// the planner.
func CreateJob(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, prompt string, autoApprove bool) (*Job, error) {
	j, err := scanJob(pool.QueryRow(ctx, createJobSQL, userID, prompt, autoApprove))
	if err != nil {
		return nil, fmt.Errorf("queue: create job: %w", err)
	}
	return j, nil
}

const createQueuedJobSQL = `
INSERT INTO jobs (user_id, prompt, status, started_at)
VALUES ($1, $2, 'QUEUED', NULL)
RETURNING ` + jobColumns

// CreateQueuedJob inserts a new job already in QUEUED, skipping
// PENDING_PLAN and PLANNING entirely — used only by tests that don't
// need a real planner run (production traffic always goes through
// CreateJob so the planner and approval gate apply).
func CreateQueuedJob(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, prompt string) (*Job, error) {
	j, err := scanJob(pool.QueryRow(ctx, createQueuedJobSQL, userID, prompt))
	if err != nil {
		return nil, fmt.Errorf("queue: create queued job: %w", err)
	}
	return j, nil
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

const setJobArtifactKeySQL = `UPDATE jobs SET artifact_key = $2 WHERE id = $1`

// SetJobArtifactKey records jobID's uploaded workspace archive. Same
// reasoning as SetJobSandboxID: a separate statement from Transition,
// since this isn't a status change (I-1 doesn't apply).
func SetJobArtifactKey(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID, artifactKey string) error {
	if _, err := pool.Exec(ctx, setJobArtifactKeySQL, jobID, artifactKey); err != nil {
		return fmt.Errorf("queue: set artifact key for job %s: %w", jobID, err)
	}
	return nil
}

const getJobSQL = `SELECT ` + jobColumns + ` FROM jobs WHERE id = $1`

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

const listJobsForUserSQL = `SELECT ` + jobColumns + ` FROM jobs WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`

// ListJobsForUser returns userID's jobs, newest first, paginated.
func ListJobsForUser(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, limit, offset int) ([]*Job, error) {
	rows, err := pool.Query(ctx, listJobsForUserSQL, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("queue: list jobs for user %s: %w", userID, err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: list jobs for user %s: scan: %w", userID, err)
		}
		jobs = append(jobs, j)
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
	j, err := scanJob(q.QueryRow(ctx, getJobSQL, jobID))
	if err != nil {
		return nil, fmt.Errorf("queue: get job %s: %w", jobID, err)
	}
	return j, nil
}
