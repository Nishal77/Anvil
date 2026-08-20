package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anvil-dev/anvil/internal/queue"
)

// This file covers Executor.RunStep's deploy path (PRD §13.1's
// DEPLOYING state) against a REAL Postgres, for the same reason
// executor_integration_test.go does: RunStep calls straight into
// internal/queue's pool-based Transition, which has no interface to
// fake.

// fakeDeployer records every Deploy call and its archive size — enough
// to prove deployPreview passes through the real uploaded bytes
// without needing a real Docker/Caddy stack in this package's tests.
type fakeDeployer struct {
	mu         sync.Mutex
	calls      []uuid.UUID
	archiveLen int
	previewURL string
	err        error
}

func (f *fakeDeployer) Deploy(_ context.Context, jobID uuid.UUID, archive []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, jobID)
	f.archiveLen = len(archive)
	if f.err != nil {
		return "", f.err
	}
	return f.previewURL, nil
}

func (f *fakeDeployer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// seedQueuedJobWithStepsAndDeploy is seedQueuedJobWithSteps plus
// options.deploy = true, with the job's status advanced to RUNNING —
// deployPreview's own RUNNING -> DEPLOYING transition requires the row
// to actually be there first, which in production is true by the time
// RunStep runs (queue.Claim already moved it QUEUED -> RUNNING before
// calling in). seedQueuedJobWithSteps alone leaves it at QUEUED,
// correct for the tests that never exercise a Transition call, but
// not for these.
func seedQueuedJobWithStepsAndDeploy(t *testing.T, pool *pgxpool.Pool) *queue.Job {
	t.Helper()
	job := seedQueuedJobWithSteps(t, pool)
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET deploy = true, status = 'RUNNING' WHERE id = $1`, job.ID); err != nil {
		t.Fatalf("set deploy option and running status: %v", err)
	}
	job.Deploy = true
	job.Status = queue.StatusRunning
	return job
}

func TestExecutor_RunStep_DeploySucceeds_TransitionsThroughDeploying(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedQueuedJobWithStepsAndDeploy(t, pool)

	sb := newFakeSandbox()
	exec := newIntegrationExecutor(t, pool, sb, stepDoneOnceProvider(t))
	exec.artifacts = &fakeArtifactStore{}
	deployer := &fakeDeployer{previewURL: "https://" + job.ID.String() + ".preview.anvil.dev"}
	exec.deployer = deployer

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep() error = %v", err)
	}

	if deployer.callCount() != 1 {
		t.Fatalf("Deploy called %d times, want 1", deployer.callCount())
	}

	got, err := queue.GetJob(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != queue.StatusSucceeded {
		t.Errorf("Status = %s, want SUCCEEDED", got.Status)
	}
	if got.PreviewURL != deployer.previewURL {
		t.Errorf("PreviewURL = %q, want %q", got.PreviewURL, deployer.previewURL)
	}
	if got.FinishedAt == nil {
		t.Error("FinishedAt is nil for a terminal job")
	}
	if got.LeaseOwner != "" {
		t.Error("LeaseOwner is still set on a terminal job")
	}
}

// TestExecutor_RunStep_DeployFails_TransitionsToFailedNotSucceeded is
// PRD §13.1's "deploy error" edge: a job that requested a preview and
// didn't get one must land in FAILED, not a silently degraded
// SUCCEEDED.
func TestExecutor_RunStep_DeployFails_TransitionsToFailedNotSucceeded(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedQueuedJobWithStepsAndDeploy(t, pool)

	sb := newFakeSandbox()
	exec := newIntegrationExecutor(t, pool, sb, stepDoneOnceProvider(t))
	exec.artifacts = &fakeArtifactStore{}
	deployer := &fakeDeployer{err: errors.New("docker build failed: base image not found")}
	exec.deployer = deployer

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep() error = %v, want nil — the failure was already written as a terminal transition", err)
	}

	got, err := queue.GetJob(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != queue.StatusFailed {
		t.Errorf("Status = %s, want FAILED", got.Status)
	}
	if got.PreviewURL != "" {
		t.Errorf("PreviewURL = %q, want empty on a failed deploy", got.PreviewURL)
	}
	if got.FailureReason == "" {
		t.Error("FailureReason is empty on a job that failed to deploy")
	}
}

// TestExecutor_RunStep_NonDeployJobNeverCallsDeployer proves a job
// that didn't request options.deploy is entirely unaffected by a
// configured Deployer — it must reach the ordinary RUNNING path,
// never touch DEPLOYING at all.
func TestExecutor_RunStep_NonDeployJobNeverCallsDeployer(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedQueuedJobWithSteps(t, pool) // Deploy left false

	sb := newFakeSandbox()
	exec := newIntegrationExecutor(t, pool, sb, stepDoneOnceProvider(t))
	exec.artifacts = &fakeArtifactStore{}
	deployer := &fakeDeployer{previewURL: "should-not-be-used"}
	exec.deployer = deployer

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep() error = %v", err)
	}
	if deployer.callCount() != 0 {
		t.Errorf("Deploy called %d times, want 0 for a job that didn't request deploy", deployer.callCount())
	}
}
