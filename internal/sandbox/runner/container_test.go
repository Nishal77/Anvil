package runner

import (
	"context"
	"fmt"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// testImage is the workspace image built locally for tests
// (images/workspace/Dockerfile, tagged anvil/workspace:test).
const testImage = "anvil/workspace:test"

// TestDockerCreateOpts_SEC001Flags checks every hardening setting one by
// one — not just that dockerCreateOpts returns something, but that a real
// container built from it actually has each setting, according to
// Docker's own inspect output. Just checking the Go struct wouldn't catch
// a setting that Docker silently ignores or renames.
func TestDockerCreateOpts_SEC001Flags(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	t.Parallel()

	docker, ctx, containerID := newTestContainer(t)

	inspect, err := docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect container: %v", err)
	}

	for _, tc := range sec001FlagChecks {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.check(inspect.Container.Config, inspect.Container.HostConfig); got != "" {
				t.Error(got)
			}
		})
	}
}

// sec001FlagChecks has one entry per hardening setting. Each check
// returns "" on success or a message describing what's wrong — written as
// small standalone functions instead of one long chain of if-statements,
// so the test stays easy to read.
var sec001FlagChecks = []struct {
	name  string
	check func(cfg *container.Config, hostCfg *container.HostConfig) string
}{
	{"NonRootUID", func(cfg *container.Config, _ *container.HostConfig) string {
		if cfg.User != "10001" {
			return fmt.Sprintf("User = %q, want %q", cfg.User, "10001")
		}
		return ""
	}},
	{"CapDropAll", func(_ *container.Config, hostCfg *container.HostConfig) string {
		if !containsString(hostCfg.CapDrop, "ALL") {
			return fmt.Sprintf("CapDrop = %v, want it to contain %q", hostCfg.CapDrop, "ALL")
		}
		return ""
	}},
	{"NoNewPrivileges", func(_ *container.Config, hostCfg *container.HostConfig) string {
		if !containsString(hostCfg.SecurityOpt, "no-new-privileges:true") {
			return fmt.Sprintf("SecurityOpt = %v, want it to contain %q", hostCfg.SecurityOpt, "no-new-privileges:true")
		}
		return ""
	}},
	{"ReadonlyRootfs", func(_ *container.Config, hostCfg *container.HostConfig) string {
		if !hostCfg.ReadonlyRootfs {
			return "ReadonlyRootfs = false, want true"
		}
		return ""
	}},
	{"PidsLimit", func(_ *container.Config, hostCfg *container.HostConfig) string {
		if hostCfg.PidsLimit == nil || *hostCfg.PidsLimit != sandboxPidsLimit {
			return fmt.Sprintf("PidsLimit = %v, want %d", hostCfg.PidsLimit, sandboxPidsLimit)
		}
		return ""
	}},
	{"Memory", func(_ *container.Config, hostCfg *container.HostConfig) string {
		if hostCfg.Memory != sandboxMemoryBytes {
			return fmt.Sprintf("Memory = %d, want %d", hostCfg.Memory, sandboxMemoryBytes)
		}
		return ""
	}},
	{"MemorySwap", func(_ *container.Config, hostCfg *container.HostConfig) string {
		if hostCfg.MemorySwap != sandboxMemoryBytes {
			return fmt.Sprintf("MemorySwap = %d, want %d (no swap headroom beyond Memory)", hostCfg.MemorySwap, sandboxMemoryBytes)
		}
		return ""
	}},
	{"NanoCPUs", func(_ *container.Config, hostCfg *container.HostConfig) string {
		if hostCfg.NanoCPUs != sandboxNanoCPUs {
			return fmt.Sprintf("NanoCPUs = %d, want %d", hostCfg.NanoCPUs, sandboxNanoCPUs)
		}
		return ""
	}},
	{"UlimitNofile", func(_ *container.Config, hostCfg *container.HostConfig) string {
		if len(hostCfg.Ulimits) != 1 || hostCfg.Ulimits[0].Name != "nofile" ||
			hostCfg.Ulimits[0].Soft != sandboxUlimitNofile || hostCfg.Ulimits[0].Hard != sandboxUlimitNofile {
			return fmt.Sprintf("Ulimits = %+v, want one nofile ulimit at %d:%d", hostCfg.Ulimits, sandboxUlimitNofile, sandboxUlimitNofile)
		}
		return ""
	}},
	{"IsolatedNetwork", func(_ *container.Config, hostCfg *container.HostConfig) string {
		if string(hostCfg.NetworkMode) != sandboxNetwork {
			return fmt.Sprintf("NetworkMode = %q, want %q (the isolated sandbox network)", hostCfg.NetworkMode, sandboxNetwork)
		}
		return ""
	}},
}

// TestCreateContainer_WorkspaceIsWritableBySandboxUser is a regression
// test for a real bug: Docker only gives its special world-writable 1777
// default to the exact path /tmp. A tmpfs mounted anywhere else —
// /workspace included — comes back owned by root, mode 0755, which the
// non-root sandbox user can't write into at all. Caught by hand running
// `mkdir /workspace/x` inside a real container and getting "Permission
// denied"; asserting the container's inspect output alone would not have
// caught this, since the bug was in what value to put in the Tmpfs
// option string, not in whether one was set at all.
func TestCreateContainer_WorkspaceIsWritableBySandboxUser(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	t.Parallel()

	docker, ctx, containerID := newTestContainer(t)

	result, err := runAndWait(ctx, docker, containerID, "mkdir", "-p", "/workspace/app")
	if err != nil {
		t.Fatalf("exec mkdir: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("mkdir /workspace/app exited %d, want 0 — the sandbox user cannot write to /workspace", result.ExitCode)
	}
}

// TestCreateContainer_WorkspaceAndTmpAreExecutable is a regression test
// for a real bug found running Week 6's executor loop against a real
// LLM: Docker's --tmpfs default is noexec. `go test` failed with
// "fork/exec ...: permission denied" trying to run its own compiled
// test binary out of /tmp, and the same would happen for any binary an
// agent writes and tries to run out of /workspace — defeating the
// sandbox's whole purpose. Asserting the container's inspect output
// alone would not catch this: HostConfig has no separate "exec" field,
// it lives inside the Tmpfs option string, same class of bug as the
// writability regression above.
func TestCreateContainer_WorkspaceAndTmpAreExecutable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	t.Parallel()

	docker, ctx, containerID := newTestContainer(t)

	for _, dir := range []string{"/workspace", "/tmp"} {
		script := dir + "/exec_test.sh"
		if _, err := runAndWait(ctx, docker, containerID, "sh", "-c", fmt.Sprintf(`printf '#!/bin/sh\necho ok\n' > %s && chmod +x %s`, script, script)); err != nil {
			t.Fatalf("write script in %s: %v", dir, err)
		}
		result, err := runAndWait(ctx, docker, containerID, script)
		if err != nil {
			t.Fatalf("exec script in %s: %v", dir, err)
		}
		if result.ExitCode != 0 {
			t.Errorf("executing a script written to %s exited %d, want 0 — noexec is still set on this tmpfs", dir, result.ExitCode)
		}
	}
}

// TestPreviewCreateOpts_SEC005SameResourceCeilingsAsSandbox proves
// SEC-005 by construction: previewCreateOpts references the exact
// same sandboxPidsLimit/sandboxMemoryBytes/sandboxNanoCPUs/
// sandboxUlimitNofile constants dockerCreateOpts uses, so this test
// only needs to catch someone copy-pasting a different literal value
// in later, not the values matching right now.
func TestPreviewCreateOpts_SEC005SameResourceCeilingsAsSandbox(t *testing.T) {
	t.Parallel()
	_, hostCfg, _ := previewCreateOpts("some-image:latest")

	for _, tc := range previewCeilingChecks {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.check(hostCfg); got != "" {
				t.Error(got)
			}
		})
	}
}

// previewCeilingChecks has one entry per SEC-005 ceiling/hardening
// setting — same pattern as sec001FlagChecks above, and for the same
// reason: keeps the test function's own branching flat.
var previewCeilingChecks = []struct {
	name  string
	check func(hostCfg *container.HostConfig) string
}{
	{"PidsLimit", func(hostCfg *container.HostConfig) string {
		if hostCfg.PidsLimit == nil || *hostCfg.PidsLimit != sandboxPidsLimit {
			return fmt.Sprintf("PidsLimit = %v, want %d", hostCfg.PidsLimit, sandboxPidsLimit)
		}
		return ""
	}},
	{"Memory", func(hostCfg *container.HostConfig) string {
		if hostCfg.Memory != sandboxMemoryBytes {
			return fmt.Sprintf("Memory = %d, want %d", hostCfg.Memory, sandboxMemoryBytes)
		}
		return ""
	}},
	{"MemorySwap", func(hostCfg *container.HostConfig) string {
		if hostCfg.MemorySwap != sandboxMemoryBytes {
			return fmt.Sprintf("MemorySwap = %d, want %d", hostCfg.MemorySwap, sandboxMemoryBytes)
		}
		return ""
	}},
	{"NanoCPUs", func(hostCfg *container.HostConfig) string {
		if hostCfg.NanoCPUs != sandboxNanoCPUs {
			return fmt.Sprintf("NanoCPUs = %d, want %d", hostCfg.NanoCPUs, sandboxNanoCPUs)
		}
		return ""
	}},
	{"UlimitNofile", func(hostCfg *container.HostConfig) string {
		if len(hostCfg.Ulimits) != 1 || hostCfg.Ulimits[0].Soft != sandboxUlimitNofile || hostCfg.Ulimits[0].Hard != sandboxUlimitNofile {
			return fmt.Sprintf("Ulimits = %+v, want one nofile ulimit at %d:%d", hostCfg.Ulimits, sandboxUlimitNofile, sandboxUlimitNofile)
		}
		return ""
	}},
	{"CapDropAll", func(hostCfg *container.HostConfig) string {
		if !containsString(hostCfg.CapDrop, "ALL") {
			return fmt.Sprintf("CapDrop = %v, want it to contain %q", hostCfg.CapDrop, "ALL")
		}
		return ""
	}},
	{"NoNewPrivileges", func(hostCfg *container.HostConfig) string {
		if !containsString(hostCfg.SecurityOpt, "no-new-privileges:true") {
			return fmt.Sprintf("SecurityOpt = %v, want it to contain %q", hostCfg.SecurityOpt, "no-new-privileges:true")
		}
		return ""
	}},
}

// newTestContainer builds a Docker client, makes sure the sandbox network
// exists, and creates one real container from dockerCreateOpts — the
// setup every test in this file that needs a live container shares.
func newTestContainer(t *testing.T) (*client.Client, context.Context, string) {
	t.Helper()
	docker, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("construct docker client: %v", err)
	}
	t.Cleanup(func() {
		if err := docker.Close(); err != nil {
			t.Errorf("cleanup: close docker client: %v", err)
		}
	})

	ctx := context.Background()
	if err := ensureSandboxNetwork(ctx, docker); err != nil {
		t.Fatalf("ensure sandbox network: %v", err)
	}

	containerID, err := createContainer(ctx, docker, testImage)
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() {
		if err := destroyContainer(context.Background(), docker, containerID); err != nil {
			t.Errorf("cleanup: destroy container: %v", err)
		}
	})

	return docker, ctx, containerID
}

// runAndWait execs command in containerID and blocks until it finishes,
// returning its inspect result (exit code included).
func runAndWait(ctx context.Context, docker *client.Client, containerID string, command ...string) (client.ExecInspectResult, error) {
	created, err := docker.ExecCreate(ctx, containerID, client.ExecCreateOptions{Cmd: command})
	if err != nil {
		return client.ExecInspectResult{}, fmt.Errorf("exec create: %w", err)
	}
	if _, err := docker.ExecStart(ctx, created.ID, client.ExecStartOptions{}); err != nil {
		return client.ExecInspectResult{}, fmt.Errorf("exec start: %w", err)
	}
	result, err := docker.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return client.ExecInspectResult{}, fmt.Errorf("exec inspect: %w", err)
	}
	return result, nil
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
