package runner

import (
	"fmt"
	"strconv"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// sandboxNetwork is the dedicated network every workspace container joins
// — it has no route to the host or to other containers. Created once at
// Server startup if it doesn't already exist; see server.go.
const sandboxNetwork = "anvil_sandbox"

const (
	sandboxUID          = 10001
	sandboxPidsLimit    = 256
	sandboxMemoryBytes  = 1 << 30       // 1 GiB
	sandboxNanoCPUs     = 1_000_000_000 // 1.0 CPU
	sandboxUlimitNofile = 1024
)

// dockerCreateOpts returns the full hardened container configuration:
// drop every Linux capability, block privilege escalation, run as a
// non-root user, make the root filesystem read-only (with writable tmpfs
// at /workspace and /tmp), cap the process count, memory, and CPU, limit
// open file handles, and join the isolated network with no route to the
// host or other containers. Every setting lives in this one function so a
// test can check each one against a real container's inspect output.
func dockerCreateOpts(image string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	cfg := &container.Config{
		Image:      image,
		User:       strconv.Itoa(sandboxUID),
		WorkingDir: "/workspace",
		Cmd:        []string{"sleep", "infinity"}, // container stays alive; exec runs actual commands
	}

	hostCfg := &container.HostConfig{
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		ReadonlyRootfs: true,
		Tmpfs: map[string]string{
			// Docker only gives its special world-writable 1777 default
			// to the exact path /tmp — a tmpfs mounted anywhere else,
			// /workspace included, comes back owned by root, mode 0755,
			// which the non-root sandbox user can't write into at all.
			// Verified directly: `mkdir /workspace/x` failed with
			// "Permission denied" until uid/gid/mode were added here.
			"/workspace": fmt.Sprintf("size=512m,uid=%d,gid=%d,mode=0755", sandboxUID, sandboxUID),
			"/tmp":       "size=512m",
		},
		NetworkMode: container.NetworkMode(sandboxNetwork),
		Resources: container.Resources{
			PidsLimit:  int64Ptr(sandboxPidsLimit),
			Memory:     sandboxMemoryBytes,
			MemorySwap: sandboxMemoryBytes, // == Memory: no swap headroom beyond the memory limit
			NanoCPUs:   sandboxNanoCPUs,
			Ulimits: []*container.Ulimit{
				{Name: "nofile", Soft: sandboxUlimitNofile, Hard: sandboxUlimitNofile},
			},
		},
	}

	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			sandboxNetwork: {},
		},
	}

	return cfg, hostCfg, netCfg
}

func int64Ptr(n int64) *int64 { return &n }
