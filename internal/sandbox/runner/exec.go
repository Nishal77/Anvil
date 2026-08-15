package runner

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/anvil-dev/anvil/internal/sandbox"
)

// execGracePeriod is how long killGroup waits after SIGTERM before
// escalating to SIGKILL.
const execGracePeriod = 10 * time.Second

const execPollInterval = 200 * time.Millisecond

// dockerExecClient is the small slice of *client.Client that runInGroup
// actually needs. Defining it here, next to where it's used, makes it
// easy to swap in a fake client for tests.
type dockerExecClient interface {
	ExecCreate(ctx context.Context, containerID string, options client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(ctx context.Context, execID string, options client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(ctx context.Context, execID string, options client.ExecInspectOptions) (client.ExecInspectResult, error)
	ExecStart(ctx context.Context, execID string, options client.ExecStartOptions) (client.ExecStartResult, error)
}

// runInGroup runs command inside containerID in its own process group and
// waits for it to exit, get cancelled, or time out — whichever happens
// first.
//
// Docker has no API to send a signal to a specific exec'd process from
// outside the container, and the Runner might not even share a process
// namespace with the Docker daemon (Docker Desktop runs containers inside
// a VM, for example). So killing a command works differently than you'd
// expect: the command runs under bash job control (`set -m`) as a
// background job, which gives it its own process group, and its process
// ID gets written to a file inside the container. To cancel or time out
// the command, we run a second, short-lived command in that *same*
// container — which does share the target's process namespace — that
// sends SIGTERM to the whole process group, waits a bit, then sends
// SIGKILL if it's still alive. Killing only the one process we exec'd
// directly wouldn't be enough — something like `bash -c 'go build'` would
// leave its child process still running.
func runInGroup(ctx context.Context, cli dockerExecClient, containerID, command string, timeout time.Duration, onChunk func(stream string, data []byte)) (exitCode int, err error) {
	// set -m is required, not optional — we tested this directly. A
	// background job only gets its own process group (which is what lets
	// killGroup target it by process group ID) when job control is
	// switched on. Without it, every process in the container shares the
	// same process group, and killing "the group" would hit everything.
	//
	// The downside is that bash prints a "[1]+ Done ..." notification
	// line to stderr when the job finishes. We tried hiding it with
	// `disown`, but that broke `wait $PID` — it returned immediately
	// instead of actually waiting for the command to finish. So instead
	// we just filter that notification line out in Go (see
	// isJobControlNotice in stream.go), which doesn't change any
	// behavior, just hides noise.
	pidFile := "/tmp/.anvil-exec-" + uuid.NewString()
	wrapped := fmt.Sprintf(`set -m; ( %s ) & PID=$!; echo $PID > %s; wait $PID`, command, pidFile)

	// We use a fresh background context here on purpose, not the ctx
	// passed into this function. ctx belongs to the caller (the incoming
	// HTTP request) and is expected to get cancelled. But if the context
	// used to open this connection gets cancelled, the connection gets
	// torn down, and Docker then throws away the exec entirely — every
	// later ExecInspect call on it fails with "No such exec instance",
	// forever, so we'd never find out how the command actually ended. We
	// hit this directly: a command's exec vanished the instant the client
	// disconnected, and the code waiting to see it finish just hung.
	// Cancelling the command is handled separately by killGroup, which
	// signals it through its own, independent exec.
	execCtx := context.Background()
	created, err := cli.ExecCreate(execCtx, containerID, client.ExecCreateOptions{ //nolint:contextcheck // reason: see comment above
		Cmd:          []string{"bash", "-c", wrapped},
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   "/workspace",
	})
	if err != nil {
		return 0, fmt.Errorf("runner: exec create: %w", err)
	}

	attached, err := cli.ExecAttach(execCtx, created.ID, client.ExecAttachOptions{}) //nolint:contextcheck // reason: see comment above
	if err != nil {
		return 0, fmt.Errorf("runner: exec attach: %w", err)
	}

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	// Buffered so the goroutine below can always send its result without
	// waiting for someone to be ready to receive it.
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := stdcopy.StdCopy(stdoutW, stderrW, attached.Reader)
		_ = stdoutW.Close()
		_ = stderrW.Close()
		copyDone <- copyErr
	}()

	scanErrs := make(chan error, 2)
	var scanWG sync.WaitGroup
	scanWG.Add(2)
	go func() { defer scanWG.Done(); scanErrs <- scanLines(stdoutR, "stdout", onChunk) }()
	go func() { defer scanWG.Done(); scanErrs <- scanLines(stderrR, "stderr", onChunk) }()

	result := waitForExecOrTimeout(ctx, cli, created.ID, containerID, pidFile, timeout, execGracePeriod)

	// We close this only after we've separately confirmed (via the polling
	// above) that the command has actually finished — not right when this
	// function returns. Docker's output stream doesn't close itself just
	// because the command exited, so waiting for the stream to close
	// before closing it ourselves would deadlock: we found this the hard
	// way when a test hung for the full shutdown timeout instead of
	// returning quickly.
	attached.Close()

	<-copyDone
	scanWG.Wait()
	close(scanErrs)
	// A truncated stream (a line over maxScanTokenBytes, most plausibly
	// — see stream.go) must not be reported as a clean, complete exit:
	// a caller trusting the output was whole would act on silently
	// missing data instead of finding out the stream was cut short.
	for err := range scanErrs {
		if err != nil {
			return result.exitCode, fmt.Errorf("runner: read command output: %w", err)
		}
	}

	if result.timedOut {
		return result.exitCode, sandbox.ErrCommandTimeout
	}
	if result.cancelled {
		return result.exitCode, fmt.Errorf("runner: exec cancelled: %w", ctx.Err())
	}
	return result.exitCode, nil
}

type execResult struct {
	exitCode            int
	timedOut, cancelled bool
}

// waitForExecOrTimeout polls the exec's status until it exits, ctx is
// cancelled, or timeout elapses. On the latter two, it kills the process
// group — waiting up to gracePeriod between SIGTERM and SIGKILL — and
// then waits for the poll to observe the actual exit; the function never
// returns while the container-side process is still running.
func waitForExecOrTimeout(ctx context.Context, cli dockerExecClient, execID, containerID, pidFile string, timeout, gracePeriod time.Duration) execResult {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// This uses a fresh background context, not ctx or timeoutCtx: it
	// needs to keep polling even after those are cancelled or expire —
	// its whole job is to confirm the command has actually stopped once
	// killGroup has done its work, not to give up early.
	done := make(chan client.ExecInspectResult, 1)                             // buffered so the goroutine below never blocks
	go func() { done <- pollUntilExited(context.Background(), cli, execID) }() //nolint:contextcheck // reason: see comment above

	select {
	case r := <-done:
		return execResult{exitCode: r.ExitCode}
	case <-timeoutCtx.Done():
		killGroup(context.Background(), cli, containerID, pidFile, gracePeriod) //nolint:contextcheck // reason: ctx and timeoutCtx are already cancelled here — this cleanup still needs to run
		r := <-done
		if ctx.Err() != nil {
			return execResult{exitCode: r.ExitCode, cancelled: true}
		}
		return execResult{exitCode: r.ExitCode, timedOut: true}
	}
}

func pollUntilExited(ctx context.Context, cli dockerExecClient, execID string) client.ExecInspectResult {
	ticker := time.NewTicker(execPollInterval)
	defer ticker.Stop()
	for {
		result, err := cli.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err == nil && !result.Running {
			return result
		}
		// The exec (or its whole container) can disappear while we're
		// still checking on it — for example if the sandbox gets
		// destroyed while a command is being cancelled. That kind of
		// error won't go away on retry, so retrying forever would just
		// hang this function and whatever's waiting on it. Treat it as
		// "the command is no longer running" instead of looping forever.
		if errdefs.IsNotFound(err) {
			return client.ExecInspectResult{}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return result
		}
	}
}

// killGroup sends SIGTERM, waits up to gracePeriod, then sends SIGKILL to
// the process group recorded in pidFile. It does this by running two more
// short commands inside containerID, since that's the only place that
// shares the target process's namespace. This is best-effort: if it
// fails, the original command is just left to be cleaned up whenever it
// eventually exits or the container gets torn down.
//
// It runs these commands detached (fire-and-forget) rather than attaching
// to read their output, because we don't care what they print, and
// attaching would bring back the same kind of stream-never-closes problem
// that runInGroup had to work around above. The wait between SIGTERM and
// SIGKILL is a plain timer instead.
func killGroup(ctx context.Context, cli dockerExecClient, containerID, pidFile string, gracePeriod time.Duration) {
	runDetached(ctx, cli, containerID, fmt.Sprintf(
		`PID=$(cat %s 2>/dev/null); [ -n "$PID" ] && kill -TERM -$PID 2>/dev/null`, pidFile))

	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}

	runDetached(ctx, cli, containerID, fmt.Sprintf(
		`PID=$(cat %s 2>/dev/null); [ -n "$PID" ] && kill -KILL -$PID 2>/dev/null`, pidFile))
}

func runDetached(ctx context.Context, cli dockerExecClient, containerID, command string) {
	created, err := cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{Cmd: []string{"bash", "-c", command}})
	if err != nil {
		return
	}
	_, _ = cli.ExecStart(ctx, created.ID, client.ExecStartOptions{Detach: true})
}
