package runner

import (
	"fmt"
	"net/netip"
	"strconv"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// sandboxNetwork is the dedicated network every workspace container joins
// — it has no route to the host or to other containers. Created once at
// Server startup if it doesn't already exist; see server.go.
const sandboxNetwork = "anvil_sandbox"

// previewNetwork is the dedicated network every preview deployment
// container joins. Unlike sandboxNetwork it is NOT internal: a preview
// exists to be reached (by Caddy, over its published host port), so
// full host isolation would defeat its purpose. It is still its own
// bridge network, isolated from sandboxNetwork and the default bridge
// — a preview container has no route to a sandbox container or vice
// versa.
const previewNetwork = "anvil_preview"

const (
	sandboxUID          = 10001
	sandboxPidsLimit    = 256
	sandboxMemoryBytes  = 1 << 30       // 1 GiB
	sandboxNanoCPUs     = 1_000_000_000 // 1.0 CPU
	sandboxUlimitNofile = 1024
)

// Preview containers reuse the sandbox's exact resource ceilings
// (SEC-005: "the same resource ceilings as the sandbox") — pids,
// memory, CPU, and open-file limits are the identical constants
// above, referenced directly in previewCreateOpts rather than
// redeclared, so the two can never silently drift apart.

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
		// HOME=/workspace: the image sets no HOME for the non-root
		// sandbox user, so it defaults to "/" — read-only. Every
		// language toolchain that caches under $HOME (go build,
		// npm, pip) fails outright without this. Not a credential
		// (SEC-020 is unaffected), just a writable home inside the
		// existing /workspace tmpfs.
		Env: []string{"HOME=/workspace"},
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
			//
			// "exec" on both: Docker's --tmpfs default is noexec, which
			// silently defeats this whole sandbox's purpose — an agent
			// whose job is to compile and run code cannot do either from
			// its own writable directories otherwise. Verified directly:
			// `go test` failed with "fork/exec ...: permission denied"
			// on its own build output under /tmp until this was added.
			// SEC-001's other controls (cap-drop ALL, no-new-privileges,
			// non-root, network isolation, resource limits) are the
			// actual containment boundary; noexec here was an unintended
			// Docker default, not a deliberate part of that boundary.
			"/workspace": fmt.Sprintf("size=512m,uid=%d,gid=%d,mode=0755,exec", sandboxUID, sandboxUID),
			"/tmp":       "size=512m,exec",
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

// previewExposedPort is the port every preview image is expected to
// listen on inside its container — fixed, not detected, because the
// Dockerfile Anvil generates when a project has none (task 9.3)
// always binds here, and a discovered Dockerfile that exposes a
// different port simply won't be reachable. Documented limitation for
// v1: PRD §9.7 doesn't specify per-project port detection, and adding
// it is a straightforward follow-up (parse the Dockerfile's own EXPOSE
// directive) if a discovered Dockerfile needs it.
var previewExposedPort = network.MustParsePort("8080/tcp")

// previewHostBindIP is loopback-only: a preview's published port is
// meant to be reached through Caddy (which runs on, or is reachable
// from, this same host), never directly from outside it.
var previewHostBindIP = netip.MustParseAddr("127.0.0.1")

// previewCreateOpts returns the container configuration for a preview
// deployment: the same resource ceilings as a sandbox (SEC-005), but
// unlike dockerCreateOpts, no fixed non-root UID or read-only root
// filesystem — the image comes from an arbitrary generated or
// discovered Dockerfile (task 9.3), not Anvil's own fixed workspace
// image, so this can't assume a UID or which paths need to be
// writable the way the sandbox's own image allows. CapDrop and
// no-new-privileges still apply; they don't depend on knowing
// anything about the image. The container's previewExposedPort is
// published to a random host port (PortBindings with HostPort ""),
// which the caller reads back from the container inspect after start.
func previewCreateOpts(image string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	cfg := &container.Config{
		Image:        image,
		ExposedPorts: network.PortSet{previewExposedPort: struct{}{}},
	}

	hostCfg := &container.HostConfig{
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges:true"},
		NetworkMode: container.NetworkMode(previewNetwork),
		PortBindings: network.PortMap{
			previewExposedPort: []network.PortBinding{{HostIP: previewHostBindIP, HostPort: ""}},
		},
		Resources: container.Resources{
			PidsLimit:  int64Ptr(sandboxPidsLimit),
			Memory:     sandboxMemoryBytes,
			MemorySwap: sandboxMemoryBytes,
			NanoCPUs:   sandboxNanoCPUs,
			Ulimits: []*container.Ulimit{
				{Name: "nofile", Soft: sandboxUlimitNofile, Hard: sandboxUlimitNofile},
			},
		},
	}

	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			previewNetwork: {},
		},
	}

	return cfg, hostCfg, netCfg
}
