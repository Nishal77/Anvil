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

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
