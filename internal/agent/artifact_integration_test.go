package agent

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
)

// fakeArtifactStore records every Upload call — enough to prove
// RunStep uploads on every terminal path without needing real object
// storage in this package's tests.
type fakeArtifactStore struct {
	mu      sync.Mutex
	uploads []uuid.UUID
}

func (f *fakeArtifactStore) Upload(_ context.Context, jobID uuid.UUID, r io.Reader, _ int64) (string, error) {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return "", fmt.Errorf("fake artifact store: drain upload: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, jobID)
	return jobID.String() + "/workspace.tar", nil
}

func (f *fakeArtifactStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

// TestExecutor_RunStep_UploadsArtifactOnSuccess proves a SUCCEEDED job
// gets its workspace uploaded, and the key is persisted on the row.
func TestExecutor_RunStep_UploadsArtifactOnSuccess(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedQueuedJobWithSteps(t, pool)

	sb := newFakeSandbox()
	exec := newIntegrationExecutor(t, pool, sb, stepDoneOnceProvider(t))
	artifacts := &fakeArtifactStore{}
	exec.artifacts = artifacts

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep() error = %v", err)
	}
	if artifacts.count() != 1 {
		t.Errorf("Upload called %d times, want 1", artifacts.count())
	}

	got, err := queue.GetJob(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.ArtifactKey == "" {
		t.Error("ArtifactKey is empty, want it set after a successful upload")
	}
}

// TestExecutor_RunStep_UploadsArtifactOnFailure proves ADR-012:
// failure preserves the artifact.
func TestExecutor_RunStep_UploadsArtifactOnFailure(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedQueuedJobWithSteps(t, pool)

	sb := newFakeSandbox()
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, false)}})
	exec := newIntegrationExecutor(t, pool, sb, provider)
	artifacts := &fakeArtifactStore{}
	exec.artifacts = artifacts

	if err := exec.RunStep(context.Background(), job); err == nil {
		t.Fatal("RunStep() error = nil, want an error for a failed step")
	}
	if artifacts.count() != 1 {
		t.Errorf("Upload called %d times, want 1 — a failed job's artifact must still be uploaded", artifacts.count())
	}
}

// TestExecutor_RunStep_UploadsArtifactOnCancel proves a cancelled
// job's artifact is uploaded too, before the sandbox is torn down.
func TestExecutor_RunStep_UploadsArtifactOnCancel(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedQueuedJobWithSteps(t, pool)

	sb := newFakeSandbox()
	exec := newIntegrationExecutor(t, pool, sb, llm.NewFakeProvider("fake"))
	exec.cancel = &countingCancelWatcher{cancelAfter: 0}
	artifacts := &fakeArtifactStore{}
	exec.artifacts = artifacts

	if err := exec.RunStep(context.Background(), job); err != nil {
		t.Fatalf("RunStep() error = %v, want nil", err)
	}
	if artifacts.count() != 1 {
		t.Errorf("Upload called %d times, want 1", artifacts.count())
	}
}
