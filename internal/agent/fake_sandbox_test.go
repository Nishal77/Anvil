package agent

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/sandbox"
)

// fakeSandbox is an in-memory sandboxManager: readlink -f is simulated
// against a configured symlink map instead of a real filesystem, and
// every other command is scripted or echoed back. It never touches
// Docker, so agent's own tests run in milliseconds — the real
// symlink-resolution proof against an actual container lives in
// test/security (TestEscape12_SymlinkPathEscapeDenied).
type fakeSandbox struct {
	mu        sync.Mutex
	symlinks  map[string]string // e.g. "/workspace/link" -> "/etc"
	execFunc  func(command string) (stdout, stderr string, exitCode int)
	created   int
	destroyed int
	commands  []string
	writes    []fakeWrite
	writeErr  error
}

type fakeWrite struct {
	path string
	data []byte
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{symlinks: map[string]string{}}
}

func (f *fakeSandbox) Create(context.Context, uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	return "fake-sandbox", nil
}

func (f *fakeSandbox) Destroy(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed++
	return nil
}

// ExportWorkspace returns an empty archive — no test in this package
// exercises artifact content, only that upload was attempted (via a
// fakeArtifactStore, where used).
func (f *fakeSandbox) ExportWorkspace(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeSandbox) Exec(_ context.Context, _ string, command string, _ time.Duration, onChunk func(sandbox.ExecChunk)) error {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.mu.Unlock()

	stdout, stderr, exitCode := f.dispatch(command)
	if stdout != "" {
		onChunk(sandbox.ExecChunk{Stream: "stdout", Data: []byte(stdout)})
	}
	if stderr != "" {
		onChunk(sandbox.ExecChunk{Stream: "stderr", Data: []byte(stderr)})
	}
	onChunk(sandbox.ExecChunk{Final: true, ExitCode: exitCode})
	return nil
}

func (f *fakeSandbox) WriteFile(_ context.Context, _, path string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, fakeWrite{path: path, data: append([]byte(nil), data...)})
	return nil
}

// dispatch simulates just enough of a shell to make resolveWorkspacePath
// and the fs_*/exec tool handlers testable: "readlink -f -- 'X'"
// resolves X through f.symlinks (longest-prefix match) and Cleans the
// rest, everything else is either a caller-supplied execFunc or a
// generic success.
func (f *fakeSandbox) dispatch(command string) (stdout, stderr string, exitCode int) {
	if f.execFunc != nil {
		return f.execFunc(command)
	}
	if strings.HasPrefix(command, "readlink -f -- ") {
		raw := strings.Trim(strings.TrimPrefix(command, "readlink -f -- "), "'")
		return f.resolveSymlink(raw) + "\n", "", 0
	}
	return "", "", 0
}

func (f *fakeSandbox) resolveSymlink(p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for target, dest := range f.symlinks {
		if p == target {
			return dest
		}
		if strings.HasPrefix(p, target+"/") {
			return dest + strings.TrimPrefix(p, target)
		}
	}
	return p
}
