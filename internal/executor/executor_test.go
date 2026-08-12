package executor

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/sandbox"
	"github.com/anvil-dev/anvil/internal/storage"
)

// fakeSandbox stands in for a real Runner connection — it records what
// was asked of it and lets a test control whether a given command
// "succeeds" or "fails," without needing a real Docker daemon for tests
// that are really about the executor's own step-sequencing and
// replay-safety logic. Real sandbox behavior is covered separately by
// internal/sandbox/runner's own tests.
type fakeSandbox struct {
	mu           sync.Mutex
	createCalls  int
	destroyCalls []string
	execCommands []string
	// failCommand, if non-empty, makes Exec report a non-zero exit for
	// any command equal to it.
	failCommand string
}

func (f *fakeSandbox) Create(context.Context, uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	return "sandbox-1", nil
}

func (f *fakeSandbox) Exec(_ context.Context, _ string, command string, _ time.Duration, onChunk func(sandbox.ExecChunk)) error {
	f.mu.Lock()
	f.execCommands = append(f.execCommands, command)
	fail := command == f.failCommand
	f.mu.Unlock()

	onChunk(sandbox.ExecChunk{Stream: "stdout", Data: []byte("output")})
	exitCode := 0
	if fail {
		exitCode = 1
	}
	onChunk(sandbox.ExecChunk{Final: true, ExitCode: exitCode})
	return nil
}

func (f *fakeSandbox) Destroy(_ context.Context, sandboxID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyCalls = append(f.destroyCalls, sandboxID)
	return nil
}

// fakePublisher records every event type published, without needing a
// real Postgres/Redis-backed events.Publisher.
type fakePublisher struct {
	mu     sync.Mutex
	events []storage.EventType
}

func (f *fakePublisher) Publish(_ context.Context, _ uuid.UUID, typ storage.EventType, _ json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, typ)
	return nil
}

func newTestExecutor(t *testing.T, sb *fakeSandbox, pub *fakePublisher) *Executor {
	t.Helper()
	e, err := New(Config{Sandbox: sb, Publisher: pub, Pool: testPool, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestJoin_HardcodedPlan_RunsThreeStepsEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job, err := queue.CreateQueuedJob(ctx, testPool, seedUser(t), "build something")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	sb := &fakeSandbox{}
	pub := &fakePublisher{}
	e := newTestExecutor(t, sb, pub)

	if err := e.RunStep(ctx, job); err != nil {
		t.Fatalf("RunStep: %v", err)
	}

	if sb.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", sb.createCalls)
	}
	if len(sb.execCommands) != len(hardcodedPlan) {
		t.Errorf("execCommands = %v, want %d commands run", sb.execCommands, len(hardcodedPlan))
	}
	if len(sb.destroyCalls) != 1 {
		t.Errorf("destroyCalls = %v, want exactly 1", sb.destroyCalls)
	}

	assertAllStepsSucceeded(t, ctx, job.ID)

	gotJob, err := queue.GetJob(ctx, testPool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if gotJob.SandboxID == "" {
		t.Error("job.SandboxID was never persisted")
	}
}

func assertAllStepsSucceeded(t *testing.T, ctx context.Context, jobID uuid.UUID) {
	t.Helper()
	for idx := range hardcodedPlan {
		step, err := queue.EnsureStep(ctx, testPool, jobID, idx, "x", "x")
		if err != nil {
			t.Fatalf("EnsureStep(%d): %v", idx, err)
		}
		if step.Status != queue.StepSucceeded {
			t.Errorf("step %d status = %q, want %q", idx, step.Status, queue.StepSucceeded)
		}
	}
}

func TestExecutor_RunStep_ResumesAfterStepAlreadySucceeded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job, err := queue.CreateQueuedJob(ctx, testPool, seedUser(t), "build something")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	// Simulate a crash after step 0 already succeeded: mark it SUCCEEDED
	// directly, as if a previous, now-dead worker got that far.
	step0, err := queue.EnsureStep(ctx, testPool, job.ID, 0, hardcodedPlan[0].Title, hardcodedPlan[0].Description)
	if err != nil {
		t.Fatalf("EnsureStep: %v", err)
	}
	if err := queue.FinishStep(ctx, testPool, step0.ID, queue.StepSucceeded, ""); err != nil {
		t.Fatalf("FinishStep: %v", err)
	}
	if err := queue.SetJobSandboxID(ctx, testPool, job.ID, "existing-sandbox"); err != nil {
		t.Fatalf("SetJobSandboxID: %v", err)
	}
	job.SandboxID = "existing-sandbox"

	sb := &fakeSandbox{}
	pub := &fakePublisher{}
	e := newTestExecutor(t, sb, pub)

	if err := e.RunStep(ctx, job); err != nil {
		t.Fatalf("RunStep: %v", err)
	}

	// A fresh sandbox must never get created when the job already has
	// one — that would silently lose whatever step 0 already did inside
	// the original container.
	if sb.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 — must reuse job.SandboxID", sb.createCalls)
	}
	// Only steps 1 and 2 should have actually run.
	if len(sb.execCommands) != len(hardcodedPlan)-1 {
		t.Errorf("execCommands = %v, want %d commands (step 0 skipped)", sb.execCommands, len(hardcodedPlan)-1)
	}
	for _, cmd := range sb.execCommands {
		if cmd == hardcodedPlan[0].Command {
			t.Errorf("step 0's command ran again: %q — replay safety broken", cmd)
		}
	}
}

func TestExecutor_RunStep_StepFailureStillDestroysSandbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job, err := queue.CreateQueuedJob(ctx, testPool, seedUser(t), "build something")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	sb := &fakeSandbox{failCommand: hardcodedPlan[0].Command}
	pub := &fakePublisher{}
	e := newTestExecutor(t, sb, pub)

	err = e.RunStep(ctx, job)
	if err == nil {
		t.Fatal("RunStep() error = nil, want an error for a failed step")
	}

	// A failed step must not leak the container: this used to return
	// early and skip the destroy call entirely.
	if len(sb.destroyCalls) != 1 {
		t.Errorf("destroyCalls = %v, want exactly 1 even though the job failed", sb.destroyCalls)
	}
	// Steps after the failed one must not have run.
	if len(sb.execCommands) != 1 {
		t.Errorf("execCommands = %v, want only the failed step to have run", sb.execCommands)
	}

	step0, err := queue.EnsureStep(ctx, testPool, job.ID, 0, "x", "x")
	if err != nil {
		t.Fatalf("EnsureStep: %v", err)
	}
	if step0.Status != queue.StepFailed {
		t.Errorf("step 0 status = %q, want %q", step0.Status, queue.StepFailed)
	}
}

func TestExecutor_RunStep_CtxCancelledLeavesSandboxRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job, err := queue.CreateQueuedJob(ctx, testPool, seedUser(t), "build something")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel() // already cancelled before RunStep even starts

	sb := &fakeSandbox{}
	pub := &fakePublisher{}
	e := newTestExecutor(t, sb, pub)

	// The step will "fail" because ctx is already cancelled — what
	// matters here isn't the error, it's that the sandbox is left alone
	// for the job's eventual resumption.
	_ = e.RunStep(cancelledCtx, job)

	if len(sb.destroyCalls) != 0 {
		t.Errorf("destroyCalls = %v, want none — a cancelled run must leave the sandbox for the next worker", sb.destroyCalls)
	}
}

func TestExecutor_New_RequiresDependencies(t *testing.T) {
	t.Parallel()
	_, err := New(Config{})
	if err == nil {
		t.Fatal("New(Config{}) error = nil, want an error for missing dependencies")
	}
}

func TestExecutor_RunStep_PublishesStepAndLogEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	job, err := queue.CreateQueuedJob(ctx, testPool, seedUser(t), "build something")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	sb := &fakeSandbox{}
	pub := &fakePublisher{}
	e := newTestExecutor(t, sb, pub)

	if err := e.RunStep(ctx, job); err != nil {
		t.Fatalf("RunStep: %v", err)
	}

	var started, finished, logLines int
	for _, typ := range pub.events {
		switch typ {
		case "step_started":
			started++
		case "step_finished":
			finished++
		case "log_line":
			logLines++
		}
	}
	if started != len(hardcodedPlan) {
		t.Errorf("step_started events = %d, want %d", started, len(hardcodedPlan))
	}
	if finished != len(hardcodedPlan) {
		t.Errorf("step_finished events = %d, want %d", finished, len(hardcodedPlan))
	}
	if logLines == 0 {
		t.Error("no log_line events published at all")
	}
}
