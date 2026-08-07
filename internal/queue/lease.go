package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const heartbeatSQL = `
UPDATE jobs SET lease_expires_at = now() + $3::interval
WHERE id = $1 AND lease_owner = $2`

// Heartbeat extends jobID's lease to now+ttl, conditional on workerID
// still owning it — checked in the WHERE clause, never read-then-compared
// in Go, which would race a concurrent reclaim. Returns ErrLeaseLost if
// workerID no longer owns it (FR-011); the caller must abandon all
// in-flight work immediately. Does not touch status, so it is not subject
// to CLAUDE.md I-1 — that invariant is about the status column.
func Heartbeat(ctx context.Context, q querier, jobID uuid.UUID, workerID string, ttl time.Duration) error {
	tag, err := q.Exec(ctx, heartbeatSQL, jobID, workerID, ttl)
	if err != nil {
		return fmt.Errorf("queue: heartbeat job %s: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}
