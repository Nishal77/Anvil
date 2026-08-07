package storage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the concrete Postgres access layer for this package's tables.
type Store struct {
	pool *pgxpool.Pool
}

// New constructs a connection pool against databaseURL, capped at
// maxConns.
func New(ctx context.Context, databaseURL string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("storage: parse database url: %w", err)
	}
	cfg.MaxConns = maxConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("storage: create connection pool: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Pool returns the shared connection pool, for domain packages (e.g.
// queue) that own their own tables and SQL rather than routing through
// Store methods. Store's job is to construct and own the one pool the
// process uses; it does not own every table in the schema.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Ping verifies database reachability, for use by readiness checks.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("storage: ping database: %w", err)
	}
	return nil
}
