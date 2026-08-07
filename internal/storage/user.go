package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// User is a row of the users table (PRD §10).
type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string // empty for OAuth-only users
	CreatedAt    time.Time
}

const pgUniqueViolation = "23505"

// CreateUser inserts a new user with the given email and Argon2id password
// hash. Returns ErrDuplicateEmail if the email is already registered.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (User, error) {
	const q = `
		INSERT INTO users (email, password_hash)
		VALUES ($1, $2)
		RETURNING id, email, password_hash, created_at`

	// email is deliberately never interpolated into these error strings —
	// it's PII (CODE-STANDARDS L5) and errors from this layer get logged
	// verbatim by the API layer (internal/api/errors.go).
	var u User
	err := s.pool.QueryRow(ctx, q, email, passwordHash).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return User{}, fmt.Errorf("create user: %w", ErrDuplicateEmail)
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

// GetUserByEmail returns the user with the given email, or ErrNotFound.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	const q = `SELECT id, email, password_hash, created_at FROM users WHERE email = $1`

	var u User
	err := s.pool.QueryRow(ctx, q, email).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, fmt.Errorf("get user by email: %w", ErrNotFound)
		}
		return User{}, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}
