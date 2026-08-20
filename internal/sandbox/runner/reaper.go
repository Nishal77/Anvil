package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

const reapInterval = time.Minute

// ensureSandboxNetwork creates the dedicated anvil_sandbox network if it
// doesn't already exist — a network with no route to the host or to other
// containers. Internal: true is what actually gives it that property; an
// internal network can't reach outside the host at all.
func ensureSandboxNetwork(ctx context.Context, docker *client.Client) error {
	existing, err := docker.NetworkList(ctx, client.NetworkListOptions{
		Filters: client.Filters{"name": {sandboxNetwork: true}},
	})
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	for _, n := range existing.Items {
		if n.Name == sandboxNetwork {
			return nil
		}
	}

	internal := true
	_, err = docker.NetworkCreate(ctx, sandboxNetwork, client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: internal,
	})
	// Two Runners can start at the same time — a restart racing the old
	// instance's shutdown, or two test processes sharing one Docker
	// daemon — and both see "doesn't exist yet" above before either has
	// created it. Whichever one loses that race just gets a conflict
	// here, which isn't a real failure: the network exists either way.
	if err != nil && !errdefs.IsConflict(err) && !errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("create network %s: %w", sandboxNetwork, err)
	}
	return nil
}

// ensureNetworks makes sure both the sandbox and preview networks
// exist — split out of Server.Run purely to keep that function's
// branching within CLAUDE.md's cyclomatic-complexity limit.
func ensureNetworks(ctx context.Context, docker *client.Client) error {
	if err := ensureSandboxNetwork(ctx, docker); err != nil {
		return fmt.Errorf("runner: ensure sandbox network: %w", err)
	}
	if err := ensurePreviewNetwork(ctx, docker); err != nil {
		return fmt.Errorf("runner: ensure preview network: %w", err)
	}
	return nil
}

// createContainer creates and starts one hardened workspace container and
// returns its ID.
func createContainer(ctx context.Context, docker *client.Client, image string) (string, error) {
	cfg, hostCfg, netCfg := dockerCreateOpts(image)

	created, err := docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           cfg,
		HostConfig:       hostCfg,
		NetworkingConfig: netCfg,
	})
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	if _, err := docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start container %s: %w", created.ID, err)
	}
	return created.ID, nil
}

// destroyContainer force-removes a container. Idempotent: removing an
// already-gone container is not an error.
func destroyContainer(ctx context.Context, docker *client.Client, containerID string) error {
	_, err := docker.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove container %s: %w", containerID, err)
	}
	return nil
}

// reapLoop periodically destroys any tracked container or preview past
// its lifetime, until ctx is cancelled. destroyAllTracked handles
// cleanup on a normal shutdown; this catches the ones that slip past
// that — a sandbox left behind by a crashed command, a client that
// never called Destroy, or a preview left running past PREVIEW_TTL
// (FR-063) because nobody ever called DELETE /previews/{job_id}.
func (s *Server) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := s.reap(ctx); err != nil {
				s.log.ErrorContext(ctx, "reap failed", "err", err)
			} else if n > 0 {
				s.log.InfoContext(ctx, "reaped stale containers", "count", n)
			}
			if n, err := s.reapPreviews(ctx); err != nil {
				s.log.ErrorContext(ctx, "reap previews failed", "err", err)
			} else if n > 0 {
				s.log.InfoContext(ctx, "reaped stale previews", "count", n)
			}
		}
	}
}

// reap destroys every tracked container whose age exceeds maxLifetime.
func (s *Server) reap(ctx context.Context) (destroyed int, err error) {
	s.mu.Lock()
	var stale []string
	now := time.Now()
	for id, createdAt := range s.containers {
		if now.Sub(createdAt) > s.maxLifetime {
			stale = append(stale, id)
		}
	}
	s.mu.Unlock()

	for _, id := range stale {
		if destroyErr := destroyContainer(ctx, s.docker, id); destroyErr != nil {
			return destroyed, fmt.Errorf("reap container %s: %w", id, destroyErr)
		}
		s.untrackContainer(id)
		destroyed++
	}
	return destroyed, nil
}

// reapPreviews destroys every tracked preview whose age exceeds
// previewTTL (FR-063, default 2h).
func (s *Server) reapPreviews(ctx context.Context) (destroyed int, err error) {
	s.mu.Lock()
	stale := make(map[string]string, len(s.previews)) // jobID -> containerID
	now := time.Now()
	for jobID, info := range s.previews {
		if now.Sub(info.createdAt) > s.previewTTL {
			stale[jobID] = info.containerID
		}
	}
	s.mu.Unlock()

	for jobID, containerID := range stale {
		if destroyErr := destroyPreview(ctx, s.docker, jobID, containerID); destroyErr != nil {
			return destroyed, fmt.Errorf("reap preview for job %s: %w", jobID, destroyErr)
		}
		s.untrackPreview(jobID)
		destroyed++
	}
	return destroyed, nil
}
