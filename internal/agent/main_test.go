package agent

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.uber.org/goleak"
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

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		fmt.Fprintln(os.Stderr, "agent tests: get connection string:", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		_ = container.Terminate(ctx)
		fmt.Fprintln(os.Stderr, "agent tests: connect:", err)
		return 1
	}

	for _, path := range []string{
		"../../migrations/001_users.up.sql",
		"../../migrations/002_jobs.up.sql",
		"../../migrations/003_steps.up.sql",
		"../../migrations/004_agent_turns.up.sql",
		"../../migrations/008_planning.up.sql",
		"../../migrations/010_create_repo.up.sql",
		"../../migrations/011_deploy.up.sql",
	} {
		sql, err := os.ReadFile(path)
		if err != nil {
			pool.Close()
			_ = container.Terminate(ctx)
			fmt.Fprintln(os.Stderr, "agent tests: read migration:", err)
			return 1
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			pool.Close()
			_ = container.Terminate(ctx)
			fmt.Fprintln(os.Stderr, "agent tests: apply migration:", err)
			return 1
		}
	}

	integrationPool = pool
	code := m.Run()

	// Close explicitly, before the leak check — not via defer. pgxpool
	// runs its own background health-check goroutine and testcontainers
	// its own reaper-connection goroutine for as long as the pool/container
	// are open; checking for leaks first and closing after (what defer
	// would do) flags those as "leaked" when they are simply still alive,
	// not abandoned by this package's code. Mirrors internal/queue/main_test.go.
	pool.Close()
	_ = container.Terminate(ctx)

	if code == 0 {
		// go.opencensus.io starts a permanent, process-lifetime stats
		// worker goroutine on package init — pulled in transitively by
		// the Docker/testcontainers client this package's tests link
		// against. It is a fixed part of that dependency, not something
		// this package's own code leaks, so it is the one function
		// goleak is told to disregard.
		if err := goleak.Find(goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start")); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			return 1
		}
	}
	return code
}

func requireIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return integrationPool
}
