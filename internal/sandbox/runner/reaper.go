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

// reapLoop periodically destroys any tracked container older than
// maxLifetime, until ctx is cancelled. destroyAllTracked handles cleanup
// on a normal shutdown; this catches the containers that slip past that
// — one left behind by a crashed command, or a client that never called
// Destroy.
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
