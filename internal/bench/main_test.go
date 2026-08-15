package bench

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// integrationPool is shared across every test in this package — the
// Runner drives queue.CreateJob/Claim/Transition directly, which have
// no interface to fake (same tradeoff internal/agent's own TestMain
// makes).
var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

// runTestMain always boots Postgres, even under -short: like
// internal/agent's own TestMain, `make coverage` runs with -short and
// skipping here would drop this package's only real tests from the
// number CLAUDE.md §5.1 gates on. testing.Short() also can't be
// queried this early — flag.Parse hasn't run yet when TestMain starts.
func runTestMain(m *testing.M) int {
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("anvil_test"),
		postgres.WithUsername("anvil"),
		postgres.WithPassword("anvil"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench tests: start postgres container:", err)
		return 1
	}
	defer func() { _ = container.Terminate(ctx) }()

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench tests: get connection string:", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench tests: connect:", err)
		return 1
	}
	defer pool.Close()

	for _, path := range []string{
		"../../migrations/001_users.up.sql",
		"../../migrations/002_jobs.up.sql",
		"../../migrations/003_steps.up.sql",
		"../../migrations/004_agent_turns.up.sql",
		"../../migrations/008_planning.up.sql",
	} {
		sql, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bench tests: read migration:", err)
			return 1
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			fmt.Fprintln(os.Stderr, "bench tests: apply migration:", err)
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
