package queue

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const sideEffectUpsertSQL = `
INSERT INTO test_side_effects (job_id, count) VALUES ($1, 1)
ON CONFLICT (job_id) DO UPDATE SET count = test_side_effects.count + 1`

// TestQueue_CrashRecovery_JobCompletesExactlyOnce proves PRD §14.3's
// crash-recovery guarantee. It launches a real OS subprocess (the
// os/exec-test "helper process" pattern) that claims a job and starts a
// fake 30-second step, SIGKILLs it mid-run — an actual unclean death, not
// a graceful ctx cancellation — then runs a fresh in-process Dispatcher
// and asserts the job resumes and finishes. Proven with a side-effect
// counter, not by reading logs.
func TestQueue_CrashRecovery_JobCompletesExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess; skipped in -short")
	}

	ctx := context.Background()
	job := seedQueuedJob(t)
	createSideEffectTable(t)

	const leaseTTL = 2 * time.Second

	cmd := startCrashTestSubprocess(t, leaseTTL)
	waitForSideEffectCount(t, job.ID, 1, 10*time.Second, func() { _ = cmd.Process.Kill() })
	killSubprocessUncleanly(t, cmd)
	assertNotYetSucceeded(t, ctx, job.ID)

	runRecoveryDispatcher(t, ctx, leaseTTL)

	waitFor(t, leaseTTL+10*time.Second, func() bool {
		row, err := getJob(ctx, testPool, job.ID)
		return err == nil && row.Status == StatusSucceeded
	}, func() {
		row, _ := getJob(ctx, testPool, job.ID)
		t.Fatalf("job never reached SUCCEEDED, last status %v", row)
	})

	// 2: one increment from the killed subprocess (started, never
	// finished), one from the recovering worker that actually completed
	// it. Not 1 (would mean the kill never landed) and not >2 (would mean
	// uncontrolled duplicate processing — recovery must claim it exactly
	// once more, not repeatedly).
	assertSideEffectCount(t, job.ID, 2)
}

func createSideEffectTable(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`CREATE TABLE IF NOT EXISTS test_side_effects (job_id UUID PRIMARY KEY, count INT NOT NULL DEFAULT 0)`)
	if err != nil {
		t.Fatalf("create side-effect table: %v", err)
	}
}

func startCrashTestSubprocess(t *testing.T, leaseTTL time.Duration) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess_CrashRecovery")
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"HELPER_DATABASE_URL="+testDSN,
		"HELPER_LEASE_TTL="+leaseTTL.String(),
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}
	return cmd
}

// waitForSideEffectCount blocks until job's side-effect counter reaches
// at least want, or calls onTimeout and fails — the side-effect counter is
// the proof a step actually started, not a fixed sleep.
func waitForSideEffectCount(t *testing.T, jobID uuid.UUID, want int, timeout time.Duration, onTimeout func()) {
	t.Helper()
	waitFor(t, timeout, func() bool {
		var count int
		_ = testPool.QueryRow(context.Background(),
			`SELECT count FROM test_side_effects WHERE job_id = $1`, jobID).Scan(&count)
		return count >= want
	}, func() {
		onTimeout()
		t.Fatal("subprocess never started the job")
	})
}

// killSubprocessUncleanly sends SIGKILL — no SIGTERM, no graceful
// shutdown. This is the "control plane crashed" scenario the durability
// design exists for.
func killSubprocessUncleanly(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL subprocess: %v", err)
	}
	_ = cmd.Wait()
}

func assertNotYetSucceeded(t *testing.T, ctx context.Context, jobID uuid.UUID) {
	t.Helper()
	row, err := getJob(ctx, testPool, jobID)
	if err != nil {
		t.Fatalf("getJob after kill: %v", err)
	}
	if row.Status == StatusSucceeded {
		t.Fatal("job already SUCCEEDED before recovery ran — the kill didn't land mid-job, this test proves nothing")
	}
}

func runRecoveryDispatcher(t *testing.T, ctx context.Context, leaseTTL time.Duration) {
	t.Helper()
	d, err := New(Config{
		Pool:              testPool,
		Logger:            testLogger(),
		WorkerID:          "recovery-worker",
		NumWorkers:        1,
		LeaseTTL:          leaseTTL,
		HeartbeatInterval: leaseTTL / 4,
		SweepInterval:     200 * time.Millisecond,
		RunStep:           sideEffectRunStep,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, leaseTTL+10*time.Second)
	done := make(chan error, 1)
	go func() { done <- d.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func assertSideEffectCount(t *testing.T, jobID uuid.UUID, want int) {
	t.Helper()
	var count int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count FROM test_side_effects WHERE job_id = $1`, jobID).Scan(&count); err != nil {
		t.Fatalf("read side-effect counter: %v", err)
	}
	if count != want {
		t.Errorf("side-effect counter = %d, want %d", count, want)
	}
}

func sideEffectRunStep(ctx context.Context, job *Job) error {
	if _, err := testPool.Exec(ctx, sideEffectUpsertSQL, job.ID); err != nil {
		return fmt.Errorf("record side effect: %w", err)
	}
	return nil
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, onTimeout func()) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			onTimeout()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHelperProcess_CrashRecovery is not a real test — it is the
// subprocess body for TestQueue_CrashRecovery_JobCompletesExactlyOnce,
// invoked via `go test -run=TestHelperProcess_CrashRecovery` with
// GO_WANT_HELPER_PROCESS=1 set. Under a normal test run (that env var
// unset) it no-ops immediately. This mirrors the standard os/exec
// TestHelperProcess pattern.
func TestHelperProcess_CrashRecovery(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		t.Skip("not invoked as a helper process")
	}

	dsn := os.Getenv("HELPER_DATABASE_URL")
	leaseTTL, err := time.ParseDuration(os.Getenv("HELPER_LEASE_TTL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: parse HELPER_LEASE_TTL:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: connect:", err)
		os.Exit(1)
	}
	defer pool.Close()

	d, err := New(Config{
		Pool:              pool,
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, nil)),
		WorkerID:          "crash-test-worker",
		NumWorkers:        1,
		LeaseTTL:          leaseTTL,
		HeartbeatInterval: leaseTTL / 4,
		SweepInterval:     time.Hour, // this process must never sweep — it IS the crash
		RunStep: func(ctx context.Context, job *Job) error {
			if _, err := pool.Exec(ctx, sideEffectUpsertSQL, job.ID); err != nil {
				return fmt.Errorf("record side effect: %w", err)
			}
			// A real subprocess simulating a real 30-second job.
			// time.Sleep is correct here — this is not a unit test
			// asserting timing, it is the live "long job" the parent
			// process kills mid-flight.
			time.Sleep(30 * time.Second)
			return nil
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: New():", err)
		os.Exit(1)
	}

	_ = d.Run(context.Background())
}
