package security

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/agent"
	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/sandbox"
)

// newPolicyEngine builds a real agent.PolicyEngine — real Registry,
// real path resolution against sandboxID inside a real Docker
// sandbox — the same construction cmd/anvil wires, minus the LLM
// pieces this file's tests don't need.
func newPolicyEngine(t *testing.T, client *sandbox.Client) *agent.PolicyEngine {
	t.Helper()
	registry, err := agent.NewRegistry(append(
		agent.NewFSTools(client),
		agent.NewExecTool(client),
		agent.NewStepDoneTool(),
	)...)
	if err != nil {
		t.Fatalf("construct registry: %v", err)
	}
	engine, err := agent.NewPolicyEngine(agent.PolicyEngineConfig{
		Registry: registry,
		Sandbox:  client,
		Budget:   llm.NewInMemoryBudgetStore(150_000),
	})
	if err != nil {
		t.Fatalf("construct policy engine: %v", err)
	}
	return engine
}

// TestEscape10_PathEscapeViaFsWrite is PRD §20.4 test 10: fs_write to
// ../../etc/passwd must be denied by the policy engine — not merely
// fail at the OS layer, but be refused before dispatch.
func TestEscape10_PathEscapeViaFsWrite(t *testing.T) {
	client, sandboxID := newSandbox(t)
	engine := newPolicyEngine(t, client)

	args, _ := json.Marshal(map[string]string{"path": "../../etc/passwd", "content": "pwned"})
	decision, reason := engine.Evaluate(context.Background(), uuid.New(), agent.ToolCall{
		Name: "fs_write", Args: args, SandboxID: sandboxID,
	})

	if decision != agent.Deny {
		t.Fatalf("Evaluate() decision = %v, want Deny", decision)
	}
	if !strings.Contains(reason, "escape") && !strings.Contains(reason, "resolves to") {
		t.Errorf("deny reason = %q, want it to describe a path escape", reason)
	}

	// Confirm nothing was actually written outside /workspace: /etc/passwd
	// on a hardened container is root-owned and read-only to begin with
	// (TestEscape01), so this is belt-and-suspenders on top of that.
	out, exitCode := runOutput(t, client, sandboxID, "grep -c pwned /etc/passwd")
	if exitCode == 0 {
		t.Fatalf("policy denial did not prevent the write: /etc/passwd now contains the injected content: %s", out)
	}
}

// TestEscape11_ProcEnvironEmpty is PRD §20.4 test 11: reading
// /proc/1/environ must reveal no secrets — SEC-020's "no ambient
// credentials in the container" claim, checked directly rather than
// through the policy engine (which would simply deny the path as an
// escape; the point here is what the container's own environment
// contains, not whether fs_read can reach it).
func TestEscape11_ProcEnvironEmpty(t *testing.T) {
	client, sandboxID := newSandbox(t)

	out, _ := runOutput(t, client, sandboxID, "cat /proc/1/environ | tr '\\0' '\\n'")
	lower := strings.ToLower(out)
	for _, secretLike := range []string{"key", "token", "secret", "password", "credential"} {
		if strings.Contains(lower, secretLike) {
			t.Errorf("/proc/1/environ contains a %q-shaped entry: %s", secretLike, out)
		}
	}
}

// TestEscape12_SymlinkPathEscapeDenied is the explicit symlink case
// the Week 6 non-negotiable calls out by name: filepath.Clean alone
// cannot catch it, because Clean is purely lexical and knows nothing
// about a symlink placed inside the workspace. This test creates
// /workspace/link -> /etc and confirms fs_read of link/passwd (a
// syntactically in-workspace path) is still denied, because
// resolution happens via readlink -f inside the sandbox, after which
// the path plainly resolves to /etc/passwd.
func TestEscape12_SymlinkPathEscapeDenied(t *testing.T) {
	client, sandboxID := newSandbox(t)
	engine := newPolicyEngine(t, client)

	if _, exitCode := runOutput(t, client, sandboxID, "ln -s /etc /workspace/link"); exitCode != 0 {
		t.Fatalf("setup: create symlink: exit code %d", exitCode)
	}

	args, _ := json.Marshal(map[string]string{"path": "link/passwd"})
	decision, reason := engine.Evaluate(context.Background(), uuid.New(), agent.ToolCall{
		Name: "fs_read", Args: args, SandboxID: sandboxID,
	})

	if decision != agent.Deny {
		t.Fatalf("Evaluate() decision = %v, want Deny for a symlink escaping the workspace (Clean(\"link/passwd\") alone would not catch this)", decision)
	}
	if !strings.Contains(reason, "/etc/passwd") && !strings.Contains(reason, "escape") {
		t.Errorf("deny reason = %q, want it to reference the resolved escape (/etc/passwd)", reason)
	}
}
