// Package integration exercises the full Week 4 join — queue, executor,
// sandbox, and events — against real infrastructure: a real Docker
// sandbox, not a fake; a real Postgres, not a mock. See
// internal/executor's own tests for the fake-sandbox unit-level coverage
// of the same step-sequencing and replay-safety logic; this package is
// what proves that logic actually works against the real thing.
package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/anvil-dev/anvil/internal/events"
	"github.com/anvil-dev/anvil/internal/executor"
	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/sandbox"
	"github.com/anvil-dev/anvil/internal/sandbox/runner"
	"github.com/anvil-dev/anvil/internal/storage"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const testImage = "anvil/workspace:test"

// alwaysDownRedis simulates Redis being completely unreachable — every
// publish fails — for TestI8_RedisDown_JobStillCompletesAndEventsQueryable.
type alwaysDownRedis struct{}

func (alwaysDownRedis) Publish(context.Context, string, []byte) error {
	return errors.New("connection refused: redis is down")
}

// newTestPostgres starts a real Postgres container with every migration
// this test needs applied, and returns a pool plus a Store built on it.
func newTestPostgres(t *testing.T) (*pgxpool.Pool, *storage.Store) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("anvil_test"),
		postgres.WithUsername("anvil"),
		postgres.WithPassword("anvil"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, path := range []string{
		"../../migrations/001_users.up.sql",
		"../../migrations/002_jobs.up.sql",
		"../../migrations/003_steps.up.sql",
		"../../migrations/005_events.up.sql",
	} {
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s: %v", path, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply migration %s: %v", path, err)
		}
	}

	store, err := storage.New(ctx, connStr, 5)
	if err != nil {
		t.Fatalf("construct store: %v", err)
	}
	t.Cleanup(store.Close)

	return pool, store
}

// newTestSandboxClient starts a real Runner backed by a real Docker
// daemon and returns a client for it — the same setup test/security uses.
func newTestSandboxClient(t *testing.T) *sandbox.Client {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release probe listener: %v", err)
	}

	srv, err := runner.New(runner.Config{
		Addr:        addr,
		Logger:      log,
		Image:       testImage,
		MaxLifetime: 5 * time.Minute,
		ExecTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("construct runner: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-runDone
	})

	client, err := sandbox.New(sandbox.Config{RunnerAddr: "http://" + addr, Logger: log})
	if err != nil {
		t.Fatalf("construct sandbox client: %v", err)
	}

	// srv.Run's HTTP listener goroutine needs a moment to actually bind
	// after New returns — retry a throwaway sandbox until it succeeds
	// instead of racing the very first real Create call against that.
	warmupID, err := waitForSandboxReady(t, client)
	if err != nil {
		t.Fatalf("runner never became ready: %v", err)
	}
	if err := client.Destroy(context.Background(), warmupID); err != nil {
		t.Fatalf("destroy warmup sandbox: %v", err)
	}

	return client
}

func waitForSandboxReady(t *testing.T, client *sandbox.Client) (string, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		id, err := client.Create(context.Background(), uuid.New())
		if err == nil {
			return id, nil
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	return "", lastErr
}

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		uuid.NewString()+"@example.com",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// TestJoin_HardcodedPlan_RunsThreeStepsEndToEnd_RealSandbox is the actual
// task-4.6 proof: POST-a-job's worth of setup, then the executor running
// all three hardcoded steps against a real container, ending with the job
// and every step persisted as SUCCEEDED.
func TestJoin_HardcodedPlan_RunsThreeStepsEndToEnd_RealSandbox(t *testing.T) {
	pool, store := newTestPostgres(t)
	exec := newTestExecutor(t, pool, store, newTestSandboxClient(t))

	job, err := queue.CreateQueuedJob(context.Background(), pool, seedUser(t, pool), "build me a thing")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep: %v", err)
	}

	gotJob, err := queue.GetJob(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if gotJob.SandboxID == "" {
		t.Error("job has no sandbox id recorded")
	}

	assertStepsSucceeded(t, pool, job.ID)
	assertLogLinePersisted(t, store, job.ID)
}

// newTestExecutor wires a real events.Publisher (backed by store and
// redis) and a real executor.Executor from it — the setup both
// integration tests in this file share.
func newTestExecutor(t *testing.T, pool *pgxpool.Pool, store *storage.Store, sandboxClient *sandbox.Client) *executor.Executor {
	t.Helper()
	pub, err := events.New(events.Config{Store: store, Redis: alwaysDownRedis{}, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("construct publisher: %v", err)
	}
	exec, err := executor.New(executor.Config{
		Sandbox:   sandboxClient,
		Publisher: pub,
		Pool:      pool,
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatalf("construct executor: %v", err)
	}
	return exec
}

func assertStepsSucceeded(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) {
	t.Helper()
	for idx := range 3 {
		step, err := queue.EnsureStep(context.Background(), pool, jobID, idx, "x", "x")
		if err != nil {
			t.Fatalf("EnsureStep(%d): %v", idx, err)
		}
		if step.Status != queue.StepSucceeded {
			t.Errorf("step %d status = %q, want %q", idx, step.Status, queue.StepSucceeded)
		}
	}
}

func assertLogLinePersisted(t *testing.T, store *storage.Store, jobID uuid.UUID) {
	t.Helper()
	gotEvents, err := store.ListEventsFrom(context.Background(), jobID, 0)
	if err != nil {
		t.Fatalf("ListEventsFrom: %v", err)
	}
	if len(gotEvents) == 0 {
		t.Fatal("no events were persisted for the job at all")
	}
	for _, ev := range gotEvents {
		if ev.Type == "log_line" {
			return
		}
	}
	t.Error("no log_line event was persisted — real container output never made it to the event log")
}

// TestI8_RedisDown_JobStillCompletesAndEventsQueryable is chaos test 7:
// with Redis entirely unreachable for the whole run, the job must still
// reach SUCCEEDED and its events must still be readable from Postgres.
// This is the strongest single durability claim in the project — Redis
// is a fast-path notification layer, never the source of truth.
func TestI8_RedisDown_JobStillCompletesAndEventsQueryable(t *testing.T) {
	pool, store := newTestPostgres(t)
	exec := newTestExecutor(t, pool, store, newTestSandboxClient(t))

	job, err := queue.CreateQueuedJob(context.Background(), pool, seedUser(t, pool), "build me a thing")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep: %v, want the job to complete even with Redis unreachable", err)
	}

	assertStepsSucceeded(t, pool, job.ID)

	got, err := store.ListEventsFrom(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatalf("ListEventsFrom: %v, want events still queryable from Postgres with Redis down", err)
	}
	if len(got) == 0 {
		t.Fatal("no events were persisted — Redis being unreachable must not stop persistence")
	}
	t.Logf("I-8 proof: %d events persisted and readable from Postgres with Redis fully unreachable", len(got))
}
