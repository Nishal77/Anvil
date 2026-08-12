package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StepStatus mirrors the step_status enum defined in migration 003.
type StepStatus string

const (
	StepPending   StepStatus = "PENDING"
	StepRunning   StepStatus = "RUNNING"
	StepSucceeded StepStatus = "SUCCEEDED"
	StepFailed    StepStatus = "FAILED"
	StepSkipped   StepStatus = "SKIPPED"
)

// Step is a row of the steps table.
type Step struct {
	ID          uuid.UUID
	JobID       uuid.UUID
	Idx         int
	Title       string
	Description string
	Status      StepStatus
	Error       string
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

const ensureStepSQL = `
INSERT INTO steps (job_id, idx, title, description)
VALUES ($1, $2, $3, $4)
ON CONFLICT (job_id, idx) DO NOTHING`

const getStepSQL = `
SELECT id, job_id, idx, title, description, status, COALESCE(error, ''), started_at, finished_at
FROM steps WHERE job_id = $1 AND idx = $2`

// EnsureStep inserts a step row for (jobID, idx) if it doesn't already
// exist, then returns the current row either way. This makes step
// creation idempotent: a worker that crashes right after creating a
// step and restarts won't duplicate the row or lose whatever status the
// step had reached before the crash — that status is exactly what tells
// the restarted worker whether to skip this step or run it again.
func EnsureStep(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID, idx int, title, description string) (Step, error) {
	if _, err := pool.Exec(ctx, ensureStepSQL, jobID, idx, title, description); err != nil {
		return Step{}, fmt.Errorf("queue: ensure step %s[%d]: %w", jobID, idx, err)
	}

	var s Step
	var typ string
	err := pool.QueryRow(ctx, getStepSQL, jobID, idx).Scan(
		&s.ID, &s.JobID, &s.Idx, &s.Title, &s.Description, &typ, &s.Error, &s.StartedAt, &s.FinishedAt,
	)
	if err != nil {
		return Step{}, fmt.Errorf("queue: ensure step %s[%d]: read back: %w", jobID, idx, err)
	}
	s.Status = StepStatus(typ)
	return s, nil
}

const startStepSQL = `
UPDATE steps SET status = 'RUNNING', started_at = COALESCE(started_at, now())
WHERE id = $1`

// StartStep marks a step RUNNING and stamps started_at, if not already set.
func StartStep(ctx context.Context, pool *pgxpool.Pool, stepID uuid.UUID) error {
	if _, err := pool.Exec(ctx, startStepSQL, stepID); err != nil {
		return fmt.Errorf("queue: start step %s: %w", stepID, err)
	}
	return nil
}

const finishStepSQL = `
UPDATE steps SET status = $2, error = NULLIF($3, ''), finished_at = now()
WHERE id = $1`

// FinishStep marks a step in its terminal status (SUCCEEDED, FAILED, or
// SKIPPED) with an optional error message and stamps finished_at.
func FinishStep(ctx context.Context, pool *pgxpool.Pool, stepID uuid.UUID, status StepStatus, stepErr string) error {
	if _, err := pool.Exec(ctx, finishStepSQL, stepID, string(status), stepErr); err != nil {
		return fmt.Errorf("queue: finish step %s: %w", stepID, err)
	}
	return nil
}
