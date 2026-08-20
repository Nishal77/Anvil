package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const testExecTimeout = 10 * time.Second

// TestWriteStdin_WritesDataToPath proves data written via writeStdin
// actually lands at path inside the container — the real end-to-end
// path SEC-020's credential injection depends on: a value handed to
// writeStdin must be exactly what a process reading path sees.
func TestWriteStdin_WritesDataToPath(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	t.Parallel()

	docker, ctx, containerID := newTestContainer(t)
	const path = "/tmp/anvil-stdin-test"

	if err := writeStdin(ctx, docker, containerID, path, []byte("secret-value")); err != nil {
		t.Fatalf("writeStdin() error = %v", err)
	}

	var got strings.Builder
	exitCode, err := runInGroup(ctx, docker, containerID, "cat "+path, testExecTimeout, func(stream string, data []byte) {
		if stream == "stdout" {
			got.WriteString(string(data))
		}
	})
	if err != nil {
		t.Fatalf("read back file: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("cat exited %d", exitCode)
	}
	if got.String() != "secret-value" {
		t.Errorf("file content = %q, want %q", got.String(), "secret-value")
	}
}

// TestWriteStdin_ThroughFIFO proves the named-pipe use case directly:
// a reader blocked on a FIFO before the write happens still receives
// the data once writeStdin runs — this is the exact shape SEC-020's
// credential-injection path needs (the git credential helper is
// already waiting on the pipe when the token arrives).
func TestWriteStdin_ThroughFIFO(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	t.Parallel()

	docker, ctx, containerID := newTestContainer(t)
	const pipePath = "/tmp/anvil-stdin-fifo-test"

	exitCode, err := runInGroup(ctx, docker, containerID, "mkfifo "+pipePath, testExecTimeout, func(string, []byte) {})
	if err != nil || exitCode != 0 {
		t.Fatalf("mkfifo failed: exitCode=%d err=%v", exitCode, err)
	}

	readDone := make(chan string, 1)
	readErr := make(chan error, 1)
	go readFIFO(docker, containerID, pipePath, readDone, readErr)

	if err := writeStdin(ctx, docker, containerID, pipePath, []byte("pipe-secret")); err != nil {
		t.Fatalf("writeStdin() error = %v", err)
	}

	select {
	case got := <-readDone:
		if got != "pipe-secret" {
			t.Errorf("reader got %q, want %q", got, "pipe-secret")
		}
	case err := <-readErr:
		t.Fatalf("fifo reader failed: %v", err)
	}
}

// readFIFO runs `cat path` inside containerID and sends its full
// stdout to done on success, or the failure reason to errCh — a
// standalone function so TestWriteStdin_ThroughFIFO's own cyclomatic
// complexity stays under CLAUDE.md's limit.
func readFIFO(docker dockerExecClient, containerID, path string, done chan<- string, errCh chan<- error) {
	var got strings.Builder
	exitCode, err := runInGroup(context.Background(), docker, containerID, "cat "+path, testExecTimeout, func(stream string, data []byte) {
		if stream == "stdout" {
			got.WriteString(string(data))
		}
	})
	if err != nil {
		errCh <- err
		return
	}
	if exitCode != 0 {
		errCh <- errors.New("fifo reader cat exited nonzero")
		return
	}
	done <- got.String()
}
