package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// fakeExecClient stands in for a real Docker connection in tests — it
// tracks just the two things waitForExecOrTimeout and pollUntilExited
// care about: whether the command is still "running", and what commands
// killGroup issued to stop it. The real Docker behavior (streaming
// output, actual process groups) is covered separately by the tests in
// container_test.go and test/security, which run against a real Docker
// daemon.
type fakeExecClient struct {
	mu       sync.Mutex
	nextID   int
	commands []string // every Cmd string passed to ExecCreate, in order

	// mainRunning is returned by ExecInspect for the main exec. A test
	// can flip it in response to a command being created, to simulate
	// the container-side process reacting to a signal.
	mainRunning bool
	notFound    bool // once true, ExecInspect returns errdefs.ErrNotFound instead

	// firstCommand is closed the moment the first ExecCreate call lands,
	// so a test can wait for "killGroup has started" instead of polling
	// in a loop with a sleep.
	firstCommandOnce sync.Once
	firstCommand     chan struct{}
}

func newFakeExecClient(mainRunning bool) *fakeExecClient {
	return &fakeExecClient{mainRunning: mainRunning, firstCommand: make(chan struct{})}
}

func (f *fakeExecClient) ExecCreate(_ context.Context, _ string, opts client.ExecCreateOptions) (client.ExecCreateResult, error) {
	f.mu.Lock()
	f.nextID++
	f.commands = append(f.commands, strings.Join(opts.Cmd, " "))
	f.mu.Unlock()
	f.firstCommandOnce.Do(func() { close(f.firstCommand) })
	return client.ExecCreateResult{ID: fmt.Sprintf("exec-%d", f.nextID)}, nil
}

func (f *fakeExecClient) ExecAttach(_ context.Context, _ string, _ client.ExecAttachOptions) (client.ExecAttachResult, error) {
	return client.ExecAttachResult{}, nil
}

func (f *fakeExecClient) ExecStart(_ context.Context, _ string, _ client.ExecStartOptions) (client.ExecStartResult, error) {
	return client.ExecStartResult{}, nil
}

func (f *fakeExecClient) ExecInspect(_ context.Context, _ string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notFound {
		return client.ExecInspectResult{}, fmt.Errorf("no such exec instance: %w", errdefs.ErrNotFound)
	}
	return client.ExecInspectResult{Running: f.mainRunning}, nil
}

func (f *fakeExecClient) commandCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commands)
}

func (f *fakeExecClient) setMainRunning(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mainRunning = v
}

// TestWaitForExecOrTimeout_NormalCompletion — the process has already
// exited, well inside the timeout, with no cancellation: killGroup must
// never run.
func TestWaitForExecOrTimeout_NormalCompletion(t *testing.T) {
	t.Parallel()
	cli := newFakeExecClient(false) // already exited when polling starts

	result := waitForExecOrTimeout(context.Background(), cli, "exec-1", "container-1", "/tmp/pf", 5*time.Second, time.Second)

	if result.timedOut || result.cancelled {
		t.Errorf("result = %+v, want neither timedOut nor cancelled", result)
	}
	if n := cli.commandCount(); n != 0 {
		t.Errorf("killGroup issued %d commands, want 0 — normal exit must not trigger a kill", n)
	}
}

// TestWaitForExecOrTimeout_TimeoutTriggersKillGroup — the process never
// exits on its own; the timeout must fire killGroup, which sends SIGTERM
// via a new exec (simulated here as immediately killing the "process",
// since the real process's reaction to the signal is exactly what the
// integration tests in test/security cover).
func TestWaitForExecOrTimeout_TimeoutTriggersKillGroup(t *testing.T) {
	t.Parallel()
	cli := newFakeExecClient(true)

	go func() {
		<-cli.firstCommand        // killGroup has issued the SIGTERM exec
		cli.setMainRunning(false) // simulate it working
	}()

	result := waitForExecOrTimeout(context.Background(), cli, "exec-1", "container-1", "/tmp/pf", 100*time.Millisecond, 10*time.Millisecond)

	if !result.timedOut {
		t.Errorf("result = %+v, want timedOut", result)
	}
	if result.cancelled {
		t.Error("result.cancelled = true, want false — this was a timeout, not a caller cancel")
	}
	if n := cli.commandCount(); n == 0 {
		t.Error("killGroup issued 0 commands, want at least the SIGTERM exec")
	}
}

// TestWaitForExecOrTimeout_CallerCancelled — ctx is cancelled by the
// caller (not a timeout): killGroup still runs (the process must still be
// killed), but the result is reported as cancelled, not timedOut.
func TestWaitForExecOrTimeout_CallerCancelled(t *testing.T) {
	t.Parallel()
	cli := newFakeExecClient(true)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		<-cli.firstCommand
		cli.setMainRunning(false)
	}()
	// The caller's cancel is independent of and typically precedes
	// killGroup's own signal — model it as a short, fixed delay via a
	// real timer rather than a test-side busy-sleep.
	timer := time.AfterFunc(20*time.Millisecond, cancel)
	t.Cleanup(func() { timer.Stop() })

	result := waitForExecOrTimeout(ctx, cli, "exec-1", "container-1", "/tmp/pf", 10*time.Second, 10*time.Millisecond)

	if !result.cancelled {
		t.Errorf("result = %+v, want cancelled", result)
	}
	if result.timedOut {
		t.Error("result.timedOut = true, want false — this was a caller cancel, not a timeout")
	}
}

// TestWaitForDrain_WaitsForSentinelOnNormalCompletion proves the fix
// for the truncation bug found live: a normal (non-killed) completion
// must wait for the drained signal before returning, not race ahead
// of it.
func TestWaitForDrain_WaitsForSentinelOnNormalCompletion(t *testing.T) {
	t.Parallel()
	drained := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		waitForDrain(execResult{}, drained)
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("waitForDrain returned before the sentinel arrived")
	case <-time.After(50 * time.Millisecond):
	}

	close(drained)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("waitForDrain did not return within 1s of the sentinel arriving")
	}
}

// TestWaitForDrain_SkipsWaitForKilledProcess proves a timed-out or
// cancelled process — which never reaches the sentinel line, since it
// was killed before or during the wrapped script — doesn't make
// waitForDrain block for the full grace timeout.
func TestWaitForDrain_SkipsWaitForKilledProcess(t *testing.T) {
	t.Parallel()
	for name, result := range map[string]execResult{
		"timed out": {timedOut: true},
		"cancelled": {cancelled: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			never := make(chan struct{}) // never closes — proves waitForDrain didn't wait on it
			done := make(chan struct{})
			go func() {
				waitForDrain(result, never)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("waitForDrain blocked on a killed process's drain signal")
			}
		})
	}
}

// TestPollUntilExited_NotFoundIsTerminal — an exec that has been
// garbage-collected (e.g. its container was destroyed mid-poll) must not
// be retried forever; that would hang the caller indefinitely instead of
// reporting the process as gone.
func TestPollUntilExited_NotFoundIsTerminal(t *testing.T) {
	t.Parallel()
	cli := newFakeExecClient(true)
	cli.notFound = true

	done := make(chan client.ExecInspectResult, 1)
	go func() { done <- pollUntilExited(context.Background(), cli, "exec-1") }()

	select {
	case result := <-done:
		if result.Running {
			t.Error("result.Running = true, want false for a not-found exec")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pollUntilExited did not return within 2s of a permanent not-found error")
	}
}
