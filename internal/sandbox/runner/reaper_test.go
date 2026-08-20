package runner

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"
)

// TestServer_ReapPreviews_DestroysExpiredButNotFreshOnes is task 9.5's
// FR-063 proof: a preview older than previewTTL is torn down, one
// created moments ago is left alone.
func TestServer_ReapPreviews_DestroysExpiredButNotFreshOnes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	t.Parallel()

	docker := newTestDockerClient(t)
	ctx := context.Background()
	if err := ensurePreviewNetwork(ctx, docker); err != nil {
		t.Fatalf("ensurePreviewNetwork: %v", err)
	}
	buildCtx := buildContextTarGz(t, busyboxHTTPDDockerfile)

	expiredJobID, expiredContainerID := buildAndRunTestPreview(t, docker, buildCtx)
	freshJobID, freshContainerID := buildAndRunTestPreview(t, docker, buildCtx)
	t.Cleanup(func() {
		_ = destroyPreview(context.Background(), docker, freshJobID, freshContainerID)
		_ = destroyPreview(context.Background(), docker, expiredJobID, expiredContainerID)
	})

	s := &Server{
		docker:     docker,
		log:        slog.New(slog.DiscardHandler),
		previewTTL: time.Hour,
		previews: map[string]previewInfo{
			expiredJobID: {containerID: expiredContainerID, createdAt: time.Now().Add(-2 * time.Hour)},
			freshJobID:   {containerID: freshContainerID, createdAt: time.Now()},
		},
	}

	destroyed, err := s.reapPreviews(ctx)
	if err != nil {
		t.Fatalf("reapPreviews: %v", err)
	}
	if destroyed != 1 {
		t.Errorf("destroyed = %d, want 1", destroyed)
	}

	if _, ok := s.lookupPreview(expiredJobID); ok {
		t.Error("expired preview is still tracked after reapPreviews")
	}
	if _, ok := s.lookupPreview(freshJobID); !ok {
		t.Error("fresh preview was reaped, want it left alone")
	}

	inspect, err := docker.ContainerInspect(ctx, expiredContainerID, client.ContainerInspectOptions{})
	if err == nil && inspect.Container.State != nil && inspect.Container.State.Running {
		t.Error("expired preview's container is still running after reapPreviews")
	}
}

// buildAndRunTestPreview builds buildCtx into a fresh image under a
// new random job ID and runs it, returning both — split out of the
// test function that calls it purely to keep that function's
// branching within CLAUDE.md's cyclomatic-complexity limit.
func buildAndRunTestPreview(t *testing.T, docker *client.Client, buildCtx []byte) (jobID, containerID string) {
	t.Helper()
	jobID = uuid.NewString()
	if err := buildPreviewImage(context.Background(), docker, jobID, bytes.NewReader(buildCtx)); err != nil {
		t.Fatalf("buildPreviewImage: %v", err)
	}
	containerID, _, err := runPreviewContainer(context.Background(), docker, jobID)
	if err != nil {
		t.Fatalf("runPreviewContainer: %v", err)
	}
	return jobID, containerID
}
