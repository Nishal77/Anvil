package queue

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.uber.org/goleak"
)

// testPool is shared across every test in this package, set up once
// against a real, containerized Postgres (CLAUDE.md T5). goleak.VerifyTestMain
// guards every goroutine this package starts (dispatcher workers, sweeper,
// heartbeat loops) — CLAUDE.md I-5, CODE-STANDARDS C7.
var (
	testPool *pgxpool.Pool
	testDSN  string
)

func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

func runTestMain(m *testing.M) int {
	// A subprocess launched by TestQueue_CrashRecovery_JobCompletesExactlyOnce
	// re-execs this same test binary. It must not spin up its own
	// container — it connects to the DSN the parent passes it and manages
	// its own pool inside TestHelperProcess_CrashRecovery.
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		return m.Run()
	}

	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("anvil_test"),
		postgres.WithUsername("anvil"),
		postgres.WithPassword("anvil"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "queue tests: start postgres container:", err)
		return 1
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "queue tests: get connection string:", err)
		return 1
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "queue tests: connect:", err)
		return 1
	}

	for _, path := range []string{
		"../../migrations/001_users.up.sql",
		"../../migrations/002_jobs.up.sql",
		"../../migrations/003_steps.up.sql",
		"../../migrations/006_idempotency.up.sql",
		"../../migrations/007_dead_letter.up.sql",
	} {
		sql, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "queue tests: read migration:", path, err)
			return 1
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			fmt.Fprintln(os.Stderr, "queue tests: apply migration:", path, err)
			return 1
		}
	}

	testPool = pool
	testDSN = connStr
	code := m.Run()

	// Close explicitly, before the leak check — not via defer. pgxpool
	// runs its own background health-check goroutine and testcontainers
	// its own reaper-connection goroutine for as long as the pool/container
	// are open; checking for leaks first and closing after (what defer
	// would do) flags those as "leaked" when they are simply still alive,
	// not abandoned by this package's code.
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

// seedUser inserts a user row, for tests that need a jobs.user_id to
// reference.
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

// seedJob inserts a claimable job in PENDING_PLAN (CreateJob's real
// production behavior) for a fresh user. Claiming it lands in PLANNING —
// which has no path to SUCCEEDED in this week's graph (that requires the
// planner/approval flow Weeks 5-7 add). Use seedQueuedJob for tests that
// need to exercise a full claim-run-succeed cycle.
func seedJob(t *testing.T) *Job {
	t.Helper()
	job, err := CreateJob(context.Background(), testPool, seedUser(t), "test prompt")
	if err != nil {
		t.Fatalf("seedJob: %v", err)
	}
	return job
}

// seedQueuedJob inserts a job already in QUEUED, as if it had already been
// planned and approved — the state a job is actually in when Week 4's
// hardcoded-plan join claims it. Claiming it lands in RUNNING, which does
// have a legal path to SUCCEEDED.
func seedQueuedJob(t *testing.T) *Job {
	t.Helper()
	job := seedJob(t)
	_, err := testPool.Exec(context.Background(),
		`UPDATE jobs SET status = 'QUEUED' WHERE id = $1`, job.ID)
	if err != nil {
		t.Fatalf("seedQueuedJob: %v", err)
	}
	job.Status = StatusQueued
	return job
}

// seedUnclaimableJob inserts a PENDING_PLAN job with run_after an hour in
// the future, set atomically at INSERT time — invisible to Claim()'s
// WHERE run_after <= now() from the instant it commits, but otherwise
// ordinary, so Transition() can still operate on it directly by ID.
//
// This must be one INSERT, not seedJob() followed by a separate UPDATE:
// under READ COMMITTED, the row becomes visible to other transactions the
// moment the INSERT commits, and CreateJob's default run_after is
// immediately claimable — a second statement to push run_after out later
// leaves a real window where a concurrent Claim() (no per-test scoping,
// t.Parallel() shares the whole table) can steal the row before the
// UPDATE lands. Seen in practice: this raced roughly 1 run in 6.
func seedUnclaimableJob(t *testing.T) *Job {
	t.Helper()
	ctx := context.Background()
	userID := seedUser(t)

	const q = `
		INSERT INTO jobs (user_id, prompt, run_after)
		VALUES ($1, $2, now() + interval '1 hour')
		RETURNING id, user_id, prompt, status, COALESCE(failure_reason, ''),
		          attempt, max_attempts, COALESCE(lease_owner, ''), lease_expires_at,
		          run_after, created_at, started_at, finished_at`

	var j Job
	err := testPool.QueryRow(ctx, q, userID, "test prompt").Scan(
		&j.ID, &j.UserID, &j.Prompt, &j.Status, &j.FailureReason,
		&j.Attempt, &j.MaxAttempts, &j.LeaseOwner, &j.LeaseExpiresAt,
		&j.RunAfter, &j.CreatedAt, &j.StartedAt, &j.FinishedAt,
	)
	if err != nil {
		t.Fatalf("seedUnclaimableJob: %v", err)
	}
	return &j
}
