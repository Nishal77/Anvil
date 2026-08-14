package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// GetIdem returns the stored result for key, if present (PRD §14.2(d)).
func (s *Store) GetIdem(ctx context.Context, key string) (json.RawMessage, bool, error) {
	const q = `SELECT result FROM idempotency_keys WHERE key = $1`

	var result json.RawMessage
	err := s.pool.QueryRow(ctx, q, key).Scan(&result)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get idempotency key %s: %w", key, err)
	}
	return result, true, nil
}

// PutIdemIfAbsent inserts result under key iff no row exists yet, and
// always returns the row's actual stored value: the caller's own
// result on a fresh insert, or the winner's on a lost race against a
// concurrent duplicate call — "a concurrent duplicate loses the race
// harmlessly" (PRD §14.2(d)).
func (s *Store) PutIdemIfAbsent(ctx context.Context, key string, jobID uuid.UUID, result json.RawMessage) (json.RawMessage, error) {
	const q = `
		INSERT INTO idempotency_keys (key, job_id, result)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET key = idempotency_keys.key
		RETURNING result`

	var stored json.RawMessage
	if err := s.pool.QueryRow(ctx, q, key, jobID, result).Scan(&stored); err != nil {
		return nil, fmt.Errorf("put idempotency key %s: %w", key, err)
	}
	return stored, nil
}
