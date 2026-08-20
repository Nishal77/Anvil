package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// previewImageTag returns the tag a preview's built image is stored
// under (FR-061: "tagged anvil-preview-{job_id}").
func previewImageTag(jobID string) string {
	return "anvil-preview-" + jobID
}

// buildPreviewImage builds buildContext (a tar or tar.gz stream — the
// job's exported workspace archive, with a Dockerfile guaranteed
// present by the caller; see internal/deploy's Dockerfile detection,
// task 9.3) into an image tagged previewImageTag(jobID).
//
// Docker's build API returns HTTP 200 and keeps streaming even when
// the build itself fails partway through — the failure shows up as an
// "error" field on one of the streamed JSON messages, never as a
// non-2xx status or a Go error from ImageBuild itself. Every message
// has to be read and checked for exactly that field, or a broken
// Dockerfile would be silently treated as a successful build.
func buildPreviewImage(ctx context.Context, docker *client.Client, jobID string, buildContext io.Reader) error {
	result, err := docker.ImageBuild(ctx, buildContext, client.ImageBuildOptions{
		Tags:       []string{previewImageTag(jobID)},
		Dockerfile: "Dockerfile",
		Remove:     true,
	})
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}
	defer func() { _ = result.Body.Close() }()

	return readBuildOutput(result.Body)
}

// buildMessage is one line of Docker's newline-delimited JSON build
// output. Only the fields this package needs to detect failure.
type buildMessage struct {
	Error string `json:"error"`
}

func readBuildOutput(body io.Reader) error {
	dec := json.NewDecoder(body)
	for {
		var msg buildMessage
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read build output: %w", err)
		}
		if msg.Error != "" {
			return fmt.Errorf("build failed: %s", msg.Error)
		}
	}
}

// runPreviewContainer starts a container from the already-built
// previewImageTag(jobID) image and returns its ID and the host port
// its previewExposedPort landed on.
func runPreviewContainer(ctx context.Context, docker *client.Client, jobID string) (containerID string, hostPort int, err error) {
	cfg, hostCfg, netCfg := previewCreateOpts(previewImageTag(jobID))

	created, err := docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           cfg,
		HostConfig:       hostCfg,
		NetworkingConfig: netCfg,
	})
	if err != nil {
		return "", 0, fmt.Errorf("create preview container: %w", err)
	}

	if _, err := docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", 0, fmt.Errorf("start preview container %s: %w", created.ID, err)
	}

	inspect, err := docker.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("inspect preview container %s: %w", created.ID, err)
	}
	bindings := inspect.Container.NetworkSettings.Ports[previewExposedPort]
	if len(bindings) == 0 {
		return "", 0, fmt.Errorf("preview container %s has no published port %s", created.ID, previewExposedPort)
	}
	hostPort, err = strconv.Atoi(bindings[0].HostPort)
	if err != nil {
		return "", 0, fmt.Errorf("parse published host port %q: %w", bindings[0].HostPort, err)
	}

	return created.ID, hostPort, nil
}

// ensurePreviewNetwork creates the anvil_preview network if it doesn't
// already exist (see previewNetwork's doc comment for why it differs
// from ensureSandboxNetwork).
func ensurePreviewNetwork(ctx context.Context, docker *client.Client) error {
	existing, err := docker.NetworkList(ctx, client.NetworkListOptions{
		Filters: client.Filters{"name": {previewNetwork: true}},
	})
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	for _, n := range existing.Items {
		if n.Name == previewNetwork {
			return nil
		}
	}

	_, err = docker.NetworkCreate(ctx, previewNetwork, client.NetworkCreateOptions{Driver: "bridge"})
	if err != nil && !errdefs.IsConflict(err) && !errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("create network %s: %w", previewNetwork, err)
	}
	return nil
}

// destroyPreview removes a preview's container and its built image.
// Idempotent: removing an already-gone container or image is not an
// error. The image is removed too (unlike a sandbox, which never
// builds one) — leaving anvil-preview-{job_id} images around forever
// would grow the Runner's disk without bound across every job that
// ever requested a preview.
func destroyPreview(ctx context.Context, docker *client.Client, jobID, containerID string) error {
	if containerID != "" {
		if _, err := docker.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("remove preview container %s: %w", containerID, err)
		}
	}
	if _, err := docker.ImageRemove(ctx, previewImageTag(jobID), client.ImageRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove preview image for job %s: %w", jobID, err)
	}
	return nil
}
