package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Secret is a row of the user_secrets table (PRD §10). Ciphertext and
// nonce are opaque to this package — encryption and decryption happen
// in internal/auth, which holds the key; storage only persists bytes.
type Secret struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Name       string
	Ciphertext []byte
	Nonce      []byte
	CreatedAt  time.Time
}

// UpsertSecret stores ciphertext and nonce under (userID, name),
// overwriting any existing value for that name — a re-submitted
// GITHUB_TOKEN replaces the old one rather than erroring.
func (s *Store) UpsertSecret(ctx context.Context, userID uuid.UUID, name string, ciphertext, nonce []byte) error {
	const q = `
		INSERT INTO user_secrets (user_id, name, ciphertext, nonce)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, name) DO UPDATE
		SET ciphertext = EXCLUDED.ciphertext, nonce = EXCLUDED.nonce`

	if _, err := s.pool.Exec(ctx, q, userID, name, ciphertext, nonce); err != nil {
		return fmt.Errorf("upsert secret: %w", err)
	}
	return nil
}

// ListSecretNames returns the names of every secret userID has stored,
// never the ciphertext — PRD §16.5: "GET /secrets returns names only."
func (s *Store) ListSecretNames(ctx context.Context, userID uuid.UUID) ([]string, error) {
	const q = `SELECT name FROM user_secrets WHERE user_id = $1 ORDER BY name`

	rows, err := s.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("list secret names: %w", err)
	}
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("list secret names: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list secret names: %w", err)
	}
	return names, nil
}

// GetSecret returns userID's stored ciphertext and nonce for name, or
// ErrNotFound. Not exposed by the API layer — this is how the Runner's
// credential-injection path (internal/auth) resolves a secret's
// plaintext for a single command invocation, never how a client reads
// one back.
func (s *Store) GetSecret(ctx context.Context, userID uuid.UUID, name string) (Secret, error) {
	const q = `SELECT id, user_id, name, ciphertext, nonce, created_at FROM user_secrets WHERE user_id = $1 AND name = $2`

	var sec Secret
	err := s.pool.QueryRow(ctx, q, userID, name).Scan(&sec.ID, &sec.UserID, &sec.Name, &sec.Ciphertext, &sec.Nonce, &sec.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Secret{}, fmt.Errorf("get secret: %w", ErrNotFound)
		}
		return Secret{}, fmt.Errorf("get secret: %w", err)
	}
	return sec, nil
}

// DeleteSecret removes userID's secret named name. Deleting a secret
// that doesn't exist is not an error — the end state the caller wants
// (no such secret) already holds.
func (s *Store) DeleteSecret(ctx context.Context, userID uuid.UUID, name string) error {
	const q = `DELETE FROM user_secrets WHERE user_id = $1 AND name = $2`

	if _, err := s.pool.Exec(ctx, q, userID, name); err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	return nil
}
