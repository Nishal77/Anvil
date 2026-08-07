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
