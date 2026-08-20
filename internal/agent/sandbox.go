package agent

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/anvil-dev/anvil/internal/sandbox"
)

// sandboxClient is the subset of *sandbox.Client the agent package
// needs, declared at the consumer (CODE-STANDARDS §3.1).
type sandboxClient interface {
	Exec(ctx context.Context, sandboxID, command string, timeout time.Duration, onChunk func(sandbox.ExecChunk)) error
	// WriteFile is used only by git_push's credential-injection path
	// (SEC-020, git.go) — it is not reachable from any tool the LLM can
	// call directly.
	WriteFile(ctx context.Context, sandboxID, path string, data []byte) error
}

// execResult is one command's collected output — buffered rather than
// streamed, because every caller in this package (path resolution,
// fs_* tools, the policy engine) needs the whole result before it can
// decide anything; none of them are the live SSE log path.
type execResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// runInSandbox executes command in sandboxID and buffers its output.
// A non-nil error means the harness could not run the command at all
// (timeout, sandbox gone) — distinct from the command running and
// exiting nonzero, which callers read from execResult.ExitCode.
func runInSandbox(ctx context.Context, client sandboxClient, sandboxID, command string, timeout time.Duration) (execResult, error) {
	var res execResult
	var stdout, stderr bytes.Buffer

	err := client.Exec(ctx, sandboxID, command, timeout, func(chunk sandbox.ExecChunk) {
		switch chunk.Stream {
		case "stdout":
			stdout.Write(chunk.Data)
		case "stderr":
			stderr.Write(chunk.Data)
		}
		if chunk.Final {
			res.ExitCode = chunk.ExitCode
		}
	})
	if err != nil {
		return execResult{}, fmt.Errorf("agent: run in sandbox: %w", err)
	}
	res.Stdout = stdout.Bytes()
	res.Stderr = stderr.Bytes()
	return res, nil
}
