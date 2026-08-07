package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RefreshToken is a row of the refresh_tokens table (PRD §10).
type RefreshToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash []byte // SHA-256, never the raw token
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// CreateRefreshToken persists a new refresh token hash for userID.
func (s *Store) CreateRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash []byte, expiresAt time.Time) (RefreshToken, error) {
	const q = `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING id, user_id, token_hash, expires_at, revoked_at`

	var rt RefreshToken
	err := s.pool.QueryRow(ctx, q, userID, tokenHash, expiresAt).
		Scan(&rt.ID, &rt.UserID, &rt.TokenHash, &rt.ExpiresAt, &rt.RevokedAt)
	if err != nil {
		return RefreshToken{}, fmt.Errorf("create refresh token for user %s: %w", userID, err)
	}
	return rt, nil
}

// GetRefreshTokenByHash returns the token matching tokenHash, or
// ErrNotFound.
func (s *Store) GetRefreshTokenByHash(ctx context.Context, tokenHash []byte) (RefreshToken, error) {
	const q = `
		SELECT id, user_id, token_hash, expires_at, revoked_at
		FROM refresh_tokens WHERE token_hash = $1`

	var rt RefreshToken
	err := s.pool.QueryRow(ctx, q, tokenHash).
		Scan(&rt.ID, &rt.UserID, &rt.TokenHash, &rt.ExpiresAt, &rt.RevokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RefreshToken{}, fmt.Errorf("get refresh token: %w", ErrNotFound)
		}
		return RefreshToken{}, fmt.Errorf("get refresh token: %w", err)
	}
	return rt, nil
}

// RevokeRefreshToken marks a refresh token revoked, idempotently.
func (s *Store) RevokeRefreshToken(ctx context.Context, id uuid.UUID) error {
	const q = `UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`

	if _, err := s.pool.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("revoke refresh token %s: %w", id, err)
	}
	return nil
}
