// Package security holds the sandbox escape tests. Each test checks one
// specific way the sandbox is supposed to contain untrusted code, running
// against a real, hardened workspace container — not a mocked Docker
// client. Only the checks that don't depend on features we haven't built
// yet are here (an egress proxy, a policy engine, network isolation
// between jobs) — those get added once those systems exist.
package security

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/sandbox"
	"github.com/anvil-dev/anvil/internal/sandbox/runner"
)

const testImage = "anvil/workspace:test"

const execTimeout = 15 * time.Second

// newSandbox starts a Runner backed by a real Docker daemon, creates one
// sandbox, and returns a client plus a cleanup func. Skipped in -short:
// this needs Docker and the anvil/workspace:test image built locally
// (make -C images/workspace build, or equivalent).
func newSandbox(t *testing.T) (*sandbox.Client, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	addr := pickAddr(t)
	srv, err := runner.New(runner.Config{
		Addr:        addr,
		Logger:      log,
		Image:       testImage,
		MaxLifetime: 5 * time.Minute,
		ExecTimeout: execTimeout,
	})
	if err != nil {
		t.Fatalf("construct runner: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	client, err := sandbox.New(sandbox.Config{RunnerAddr: "http://" + addr, Logger: log})
	if err != nil {
		cancel()
		t.Fatalf("construct client: %v", err)
	}

	sandboxID, err := waitForCreate(t, client)
	if err != nil {
		cancel()
		t.Fatalf("create sandbox: %v", err)
	}

	t.Cleanup(func() {
		_ = client.Destroy(context.Background(), sandboxID)
		cancel()
		<-runDone
	})

	return client, sandboxID
}

// waitForCreate retries Create briefly: the HTTP listener goroutine in
// srv.Run needs a moment to bind after New returns.
func waitForCreate(t *testing.T, client *sandbox.Client) (string, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		id, err := client.Create(context.Background(), uuid.New())
		if err == nil {
			return id, nil
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	return "", lastErr
}

// pickAddr finds a free TCP port so the parallel tests in this package —
// each starting its own Runner instance — don't collide on a fixed one.
func pickAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release probe listener: %v", err)
	}
	return addr
}

// runOutput execs command and returns its combined stdout+stderr and exit
// code.
func runOutput(t *testing.T, client *sandbox.Client, sandboxID, command string) (output string, exitCode int) {
	t.Helper()
	var sb strings.Builder
	err := client.Exec(context.Background(), sandboxID, command, execTimeout, func(c sandbox.ExecChunk) {
		if c.Final {
			exitCode = c.ExitCode
			return
		}
		sb.Write(c.Data)
		sb.WriteByte('\n')
	})
	if err != nil {
		t.Fatalf("exec %q: %v", command, err)
	}
	return sb.String(), exitCode
}

// TestEscape01_ShadowFileDenied checks that reading /etc/shadow fails
// with a permission error. The container runs as a non-root user with a
// read-only root filesystem, and /etc/shadow is only readable by root.
func TestEscape01_ShadowFileDenied(t *testing.T) {
	t.Parallel()
	client, sandboxID := newSandbox(t)

	output, exitCode := runOutput(t, client, sandboxID, "cat /etc/shadow")

	if exitCode == 0 {
		t.Fatalf("cat /etc/shadow succeeded, want non-zero exit; output: %s", output)
	}
	if !strings.Contains(output, "Permission denied") {
		t.Errorf("output = %q, want a permission-denied error", output)
	}
}

// TestEscape02_DockerCLIAbsent checks that the docker command doesn't
// exist inside the workspace image at all.
func TestEscape02_DockerCLIAbsent(t *testing.T) {
	t.Parallel()
	client, sandboxID := newSandbox(t)

	output, exitCode := runOutput(t, client, sandboxID, "docker ps")

	if exitCode == 0 {
		t.Fatalf("docker ps succeeded, want command-not-found; output: %s", output)
	}
	if !strings.Contains(output, "not found") {
		t.Errorf("output = %q, want a command-not-found error", output)
	}
}

// TestEscape03_DockerSocketAbsent checks that the Docker socket is never
// mounted into a workspace container.
func TestEscape03_DockerSocketAbsent(t *testing.T) {
	t.Parallel()
	client, sandboxID := newSandbox(t)

	output, exitCode := runOutput(t, client, sandboxID, "ls /var/run/docker.sock")

	if exitCode == 0 {
		t.Fatalf("docker.sock exists, want no such file; output: %s", output)
	}
	if !strings.Contains(output, "No such file") {
		t.Errorf("output = %q, want a no-such-file error", output)
	}
}

// TestEscape04_SudoAbsent checks there's no way to gain root privileges
// from inside the container.
func TestEscape04_SudoAbsent(t *testing.T) {
	t.Parallel()
	client, sandboxID := newSandbox(t)

	output, exitCode := runOutput(t, client, sandboxID, "sudo -i")

	if exitCode == 0 {
		t.Fatalf("sudo -i succeeded, want command-not-found; output: %s", output)
	}
	if !strings.Contains(output, "not found") {
		t.Errorf("output = %q, want a command-not-found error", output)
	}
}

// TestEscape05_RemountDenied checks that the read-only root filesystem
// can't be remounted as writable. Two separate protections back this up:
// the non-root user can't run the mount command at all ("must be
// superuser"), and even a root user in the container would be blocked
// separately, since every Linux capability that would allow it has been
// dropped. Either denial message proves the container is contained —
// only an actual successful remount would be a real problem.
func TestEscape05_RemountDenied(t *testing.T) {
	t.Parallel()
	client, sandboxID := newSandbox(t)

	output, exitCode := runOutput(t, client, sandboxID, "mount -o remount,rw / 2>&1")

	if exitCode == 0 {
		t.Fatalf("remount succeeded, want operation-not-permitted; output: %s", output)
	}
	denied := strings.Contains(output, "not permitted") ||
		strings.Contains(output, "superuser") ||
		strings.Contains(output, "not found")
	if !denied {
		t.Errorf("output = %q, want a permission-denied error", output)
	}
}

// TestEscape06_ForkBombContained checks that a fork bomb — a program that
// spawns copies of itself until it exhausts system resources — is
// contained by the container's process-count limit. The command should
// still return, and just as importantly, Docker itself and a brand new
// sandbox should keep working fine afterward.
func TestEscape06_ForkBombContained(t *testing.T) {
	t.Parallel()
	client, sandboxID := newSandbox(t)

	start := time.Now()
	_, exitCode := runOutput(t, client, sandboxID, `:(){ :|:& };:`)
	elapsed := time.Since(start)

	if elapsed > execTimeout {
		t.Fatalf("fork bomb exec took %s, want contained well under the %s timeout", elapsed, execTimeout)
	}
	t.Logf("fork bomb exec: exit=%d after %s (pids-limit containment, not necessarily 0)", exitCode, elapsed)

	probeSandboxID, err := waitForCreate(t, client)
	if err != nil {
		t.Fatalf("host/daemon unresponsive after fork bomb: create failed: %v", err)
	}
	t.Cleanup(func() { _ = client.Destroy(context.Background(), probeSandboxID) })

	probeStart := time.Now()
	output, exitCode := runOutput(t, client, probeSandboxID, "echo still-alive")
	if exitCode != 0 || !strings.Contains(output, "still-alive") {
		t.Fatalf("host unresponsive after fork bomb: probe exec output=%q exit=%d", output, exitCode)
	}
	t.Logf("host responsive: probe exec completed in %s", time.Since(probeStart))
}
