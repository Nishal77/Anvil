package sandbox

import (
	"time"

	"github.com/google/uuid"
)

// CreateRequest asks the Runner to create one sandbox container for a job.
type CreateRequest struct {
	JobID uuid.UUID `json:"job_id"`
}

// CreateResponse identifies the created sandbox.
type CreateResponse struct {
	SandboxID string `json:"sandbox_id"`
}

// ExecRequest runs one command inside an existing sandbox.
type ExecRequest struct {
	SandboxID string `json:"sandbox_id"`
	Command   string `json:"command"`
	TimeoutS  int    `json:"timeout_s"` // 0 means use the Runner's default (300s)
}

// ExecChunk is one small piece of a running command's output, sent as one
// line of JSON the moment it's produced — never held back until the
// command finishes.
type ExecChunk struct {
	Stream    string    `json:"stream"` // "stdout" | "stderr"
	Data      []byte    `json:"data"`
	Timestamp time.Time `json:"timestamp"`
	Final     bool      `json:"final"` // true on the chunk carrying ExitCode
	ExitCode  int       `json:"exit_code,omitempty"`
}

// DestroyRequest tears down a sandbox immediately.
type DestroyRequest struct {
	SandboxID string `json:"sandbox_id"`
}

// BuildPreviewResponse is returned once a preview image has built and
// its container is running (PRD §9.7, FR-060/FR-061). HostPort is
// where the container's exposed port landed on the Runner's own
// host — the control plane uses it to register a Caddy route, never
// exposes it to a client directly.
type BuildPreviewResponse struct {
	ContainerID string `json:"container_id"`
	HostPort    int    `json:"host_port"`
}

// WriteRequest writes data to path inside an existing sandbox — the
// control-plane side of the named-pipe credential injection SEC-020
// requires: a git credential helper inside the sandbox reads a secret
// from a path this writes to, so the secret never becomes an
// environment variable or a file the container's own filesystem
// layer persists (PRD §16.5). Not part of the agent tool registry —
// the LLM never gets to call this; only internal/agent's Go code does,
// with a path it generates itself.
type WriteRequest struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
	Data      []byte `json:"data"`
}
