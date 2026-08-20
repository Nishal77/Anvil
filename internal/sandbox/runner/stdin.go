package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/moby/moby/client"
)

// writeStdinTimeout bounds writeStdin — this only ever moves a few
// hundred bytes (a credential), so a short, fixed timeout is enough;
// callers don't need to tune it.
const writeStdinTimeout = 10 * time.Second

// writeStdin runs `cat > path` inside containerID and writes data to
// that process's stdin, then half-closes the connection's write side
// so cat sees EOF and exits — this is the write side of SEC-020's
// named-pipe credential injection: path is expected to be a FIFO a
// caller already created inside the container (mkfifo), so the bytes
// written here never land in the container's persisted filesystem
// layer, unlike writing to a regular file.
func writeStdin(ctx context.Context, cli dockerExecClient, containerID, path string, data []byte) error {
	execCtx, cancel := context.WithTimeout(ctx, writeStdinTimeout)
	defer cancel()

	created, err := cli.ExecCreate(execCtx, containerID, client.ExecCreateOptions{
		Cmd:          []string{"bash", "-c", "cat > " + path},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("runner: write stdin: exec create: %w", err)
	}

	attached, err := cli.ExecAttach(execCtx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("runner: write stdin: exec attach: %w", err)
	}
	defer attached.Close()

	// cat produces no output of its own, but the attach connection
	// still has to be read from — Docker's internal buffer for the
	// unread side can fill and block the write below otherwise.
	drainDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, attached.Reader)
		close(drainDone)
	}()

	if _, err := attached.Conn.Write(data); err != nil {
		return fmt.Errorf("runner: write stdin: write data: %w", err)
	}
	if err := attached.CloseWrite(); err != nil {
		return fmt.Errorf("runner: write stdin: close write side: %w", err)
	}

	result := pollUntilExited(execCtx, cli, created.ID)
	<-drainDone

	if result.Running {
		return errors.New("runner: write stdin: timed out waiting for cat to exit")
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("runner: write stdin: cat exited %d", result.ExitCode)
	}
	return nil
}
