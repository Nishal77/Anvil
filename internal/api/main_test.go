package api

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.uber.org/goleak"
)

// testPool is shared across every test in this package that needs a real
// database — the job handlers call straight into queue's functions,
// which take a concrete *pgxpool.Pool rather than an interface, so there
// is no faking this one away (CLAUDE.md T5: never mock the database).
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
		fmt.Fprintln(os.Stderr, "api tests: start postgres container:", err)
		return 1
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "api tests: get connection string:", err)
		return 1
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "api tests: connect:", err)
		return 1
	}

	for _, path := range []string{
		"../../migrations/001_users.up.sql",
		"../../migrations/002_jobs.up.sql",
		"../../migrations/003_steps.up.sql",
		"../../migrations/005_events.up.sql",
		"../../migrations/008_planning.up.sql",
		"../../migrations/010_create_repo.up.sql",
		"../../migrations/011_deploy.up.sql",
	} {
		sql, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "api tests: read migration:", path, err)
			return 1
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			fmt.Fprintln(os.Stderr, "api tests: apply migration:", path, err)
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
