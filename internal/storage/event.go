package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// EventType mirrors the event_type enum defined in migration 005.
type EventType string

// Event is a row of job_events.
type Event struct {
	JobID     uuid.UUID
	Seq       int64
	Type      EventType
	Payload   json.RawMessage
	CreatedAt time.Time
}

// AppendEvent allocates the next seq for jobID from job_event_seq and
// inserts the event, in one transaction. That's what keeps the persisted
// record gap-free: a crash between allocating a seq and writing the event
// row is impossible, because both happen in the same commit.
func (s *Store) AppendEvent(ctx context.Context, jobID uuid.UUID, typ EventType, payload json.RawMessage) (Event, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("append event: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const allocSeqSQL = `
		INSERT INTO job_event_seq (job_id, last_seq)
		VALUES ($1, 1)
		ON CONFLICT (job_id) DO UPDATE SET last_seq = job_event_seq.last_seq + 1
		RETURNING last_seq`

	var seq int64
	if err := tx.QueryRow(ctx, allocSeqSQL, jobID).Scan(&seq); err != nil {
		return Event{}, fmt.Errorf("append event: allocate seq: %w", err)
	}

	const insertSQL = `
		INSERT INTO job_events (job_id, seq, type, payload)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at`

	var createdAt time.Time
	if err := tx.QueryRow(ctx, insertSQL, jobID, seq, string(typ), payload).Scan(&createdAt); err != nil {
		return Event{}, fmt.Errorf("append event: insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Event{}, fmt.Errorf("append event: commit: %w", err)
	}

	return Event{JobID: jobID, Seq: seq, Type: typ, Payload: payload, CreatedAt: createdAt}, nil
}

// ListEventsFrom returns jobID's events with seq > fromSeq, ascending —
// used both for SSE resume (Last-Event-ID / ?from_seq=) and for a client
// re-fetching a stream_gap's missing range.
func (s *Store) ListEventsFrom(ctx context.Context, jobID uuid.UUID, fromSeq int64) ([]Event, error) {
	const q = `
		SELECT job_id, seq, type, payload, created_at
		FROM job_events
		WHERE job_id = $1 AND seq > $2
		ORDER BY seq`

	rows, err := s.pool.Query(ctx, q, jobID, fromSeq)
	if err != nil {
		return nil, fmt.Errorf("list events from %d: %w", fromSeq, err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var typ string
		if err := rows.Scan(&e.JobID, &e.Seq, &typ, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("list events from %d: scan: %w", fromSeq, err)
		}
		e.Type = EventType(typ)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events from %d: %w", fromSeq, err)
	}
	return events, nil
}
