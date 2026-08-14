package agent

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// integrationPool is shared across every test in this package that
// needs a real Postgres (Executor.RunStep and everything it drives
// call straight into internal/queue's pool-based helpers, which have
// no interface to fake). Booted once here, not per-test: a fresh
// container per test would make this suite too slow to run on every
// commit (same tradeoff internal/storage's TestMain makes).
//
// Not gated behind testing.Short(): internal/storage's own TestMain
// boots Postgres unconditionally too, and `make coverage` runs with
// -short — skipping here would silently drop this package's biggest
// coverage contributor from the number CLAUDE.md §5.1 gates on.
var integrationPool *pgxpool.Pool

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
		fmt.Fprintln(os.Stderr, "agent tests: start postgres container:", err)
		return 1
	}
	defer func() { _ = container.Terminate(ctx) }()

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent tests: get connection string:", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent tests: connect:", err)
		return 1
	}
	defer pool.Close()

	for _, path := range []string{
		"../../migrations/001_users.up.sql",
		"../../migrations/002_jobs.up.sql",
		"../../migrations/003_steps.up.sql",
		"../../migrations/004_agent_turns.up.sql",
	} {
		sql, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "agent tests: read migration:", err)
			return 1
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			fmt.Fprintln(os.Stderr, "agent tests: apply migration:", err)
			return 1
		}
	}

	integrationPool = pool
	return m.Run()
}

func requireIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return integrationPool
}
