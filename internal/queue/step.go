package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	// Acceptance is how the executor knows this step succeeded (PRD
	// §12.1) — surfaced to the model in the context builder's tier 3.
	Acceptance string
	// Optional, if true, means repair-exhaustion SKIPs this step
	// instead of failing the whole job (PRD §12.4).
	Optional    bool
	Status      StepStatus
	Error       string
	RepairCount int
	TurnCount   int
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

const stepColumns = `id, job_id, idx, title, description, acceptance, optional,
	status, COALESCE(error, ''), repair_count, turn_count, started_at, finished_at`

func scanStep(row pgx.Row) (Step, error) {
	var s Step
	var status string
	err := row.Scan(
		&s.ID, &s.JobID, &s.Idx, &s.Title, &s.Description, &s.Acceptance, &s.Optional,
		&status, &s.Error, &s.RepairCount, &s.TurnCount, &s.StartedAt, &s.FinishedAt,
	)
	if err != nil {
		return Step{}, fmt.Errorf("queue: scan step: %w", err)
	}
	s.Status = StepStatus(status)
	return s, nil
}

const ensureStepSQL = `
INSERT INTO steps (job_id, idx, title, description)
VALUES ($1, $2, $3, $4)
ON CONFLICT (job_id, idx) DO NOTHING`

const getStepSQL = `SELECT ` + stepColumns + ` FROM steps WHERE job_id = $1 AND idx = $2`

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

	s, err := scanStep(pool.QueryRow(ctx, getStepSQL, jobID, idx))
	if err != nil {
		return Step{}, fmt.Errorf("queue: ensure step %s[%d]: read back: %w", jobID, idx, err)
	}
	return s, nil
}

const listStepsSQL = `SELECT ` + stepColumns + ` FROM steps WHERE job_id = $1 ORDER BY idx`

// ListSteps returns jobID's steps in plan order — the order SavePlan
// wrote them and the order the executor must run them in.
func ListSteps(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) ([]Step, error) {
	rows, err := pool.Query(ctx, listStepsSQL, jobID)
	if err != nil {
		return nil, fmt.Errorf("queue: list steps for job %s: %w", jobID, err)
	}
	defer rows.Close()

	var steps []Step
	for rows.Next() {
		s, err := scanStep(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: list steps for job %s: scan: %w", jobID, err)
		}
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: list steps for job %s: %w", jobID, err)
	}
	return steps, nil
}

const incrementRepairCountSQL = `UPDATE steps SET repair_count = repair_count + 1 WHERE id = $1 RETURNING repair_count`

// IncrementRepairCount records one more repair attempt on stepID and
// returns the new count. Persisted on the row, not held in memory —
// repair accounting must survive a crash-reclaim cycle, or a
// crash-repair-crash loop never terminates (FR-022).
func IncrementRepairCount(ctx context.Context, pool *pgxpool.Pool, stepID uuid.UUID) (int, error) {
	var count int
	if err := pool.QueryRow(ctx, incrementRepairCountSQL, stepID).Scan(&count); err != nil {
		return 0, fmt.Errorf("queue: increment repair count for step %s: %w", stepID, err)
	}
	return count, nil
}

const incrementTurnCountSQL = `UPDATE steps SET turn_count = turn_count + 1 WHERE id = $1`

// IncrementTurnCount records one more executor turn spent on stepID.
func IncrementTurnCount(ctx context.Context, pool *pgxpool.Pool, stepID uuid.UUID) error {
	if _, err := pool.Exec(ctx, incrementTurnCountSQL, stepID); err != nil {
		return fmt.Errorf("queue: increment turn count for step %s: %w", stepID, err)
	}
	return nil
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
	jobStepsTotal.WithLabelValues(string(status)).Inc()
	return nil
}
