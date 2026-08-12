package executor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.uber.org/goleak"
)

// testPool is shared across every test in this package, set up once
// against a real, containerized Postgres — the executor's replay-safety
// guarantees (skip a step already SUCCEEDED, reuse an existing sandbox)
// are exactly the kind of thing that looks right against a mock and
// isn't.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

func runTestMain(m *testing.M) int {
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("anvil_test"),
		postgres.WithUsername("anvil"),
		postgres.WithPassword("anvil"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "executor tests: start postgres container:", err)
		return 1
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "executor tests: get connection string:", err)
		return 1
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "executor tests: connect:", err)
		return 1
	}

	for _, path := range []string{
		"../../migrations/001_users.up.sql",
		"../../migrations/002_jobs.up.sql",
		"../../migrations/003_steps.up.sql",
	} {
		sql, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "executor tests: read migration:", path, err)
			return 1
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			fmt.Fprintln(os.Stderr, "executor tests: apply migration:", path, err)
			return 1
		}
	}

	testPool = pool
	code := m.Run()

	pool.Close()
	_ = container.Terminate(ctx)

	if code == 0 {
		if err := goleak.Find(); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			return 1
		}
	}
	return code
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func seedUser(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := testPool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		uuid.NewString()+"@example.com",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedUser: %v", err)
	}
	return id
}
