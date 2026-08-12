package queue

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestJob_CreateQueuedJob_StartsInQueued(t *testing.T) {
	t.Parallel()
	job, err := CreateQueuedJob(context.Background(), testPool, seedUser(t), "build me a thing")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}
	if job.Status != StatusQueued {
		t.Errorf("Status = %q, want %q", job.Status, StatusQueued)
	}
	if job.StartedAt != nil {
		t.Error("StartedAt is set, want nil until a worker actually claims it")
	}
}

func TestJob_GetJob_NotFoundReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	_, err := GetJob(context.Background(), testPool, uuid.New())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetJob() error = %v, want ErrNotFound", err)
	}
}

func TestJob_GetJob_RoundTrip(t *testing.T) {
	t.Parallel()
	created := seedJob(t)

	got, err := GetJob(context.Background(), testPool, created.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.ID != created.ID || got.Prompt != created.Prompt {
		t.Errorf("GetJob() = %+v, want ID=%s Prompt=%q", got, created.ID, created.Prompt)
	}
}

func TestJob_ListJobsForUser_NewestFirstAndScopedToUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	userA := seedUser(t)
	userB := seedUser(t)

	jobA1, err := CreateQueuedJob(ctx, testPool, userA, "a1")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}
	jobA2, err := CreateQueuedJob(ctx, testPool, userA, "a2")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}
	if _, err := CreateQueuedJob(ctx, testPool, userB, "b1"); err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	jobs, err := ListJobsForUser(ctx, testPool, userA, 10, 0)
	if err != nil {
		t.Fatalf("ListJobsForUser: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("ListJobsForUser(userA) returned %d jobs, want 2", len(jobs))
	}
	if jobs[0].ID != jobA2.ID || jobs[1].ID != jobA1.ID {
		t.Errorf("ListJobsForUser(userA) = [%s, %s], want [%s, %s] (newest first)",
			jobs[0].ID, jobs[1].ID, jobA2.ID, jobA1.ID)
	}
}

func TestJob_SetJobSandboxID_Persists(t *testing.T) {
	t.Parallel()
	job := seedJob(t)

	if err := SetJobSandboxID(context.Background(), testPool, job.ID, "container-abc123"); err != nil {
		t.Fatalf("SetJobSandboxID: %v", err)
	}

	got, err := GetJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.SandboxID != "container-abc123" {
		t.Errorf("SandboxID = %q, want %q", got.SandboxID, "container-abc123")
	}
}
