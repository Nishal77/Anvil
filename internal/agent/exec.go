package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	defaultExecTimeoutS = 60
	maxExecTimeoutS     = 300 // SEC-... / FR-041's per-command ceiling
)

// NewExecTool returns the GUARDED exec tool bound to client. Policy
// rule 4 (blocklist) and the credential-shaped-string check run in
// PolicyEngine.Evaluate, before this Handler ever runs — this Handler
// trusts that a call reaching it already passed policy.
func NewExecTool(client sandboxClient) Tool {
	return Tool{
		Name:        "exec",
		Description: "Run a shell command in the workspace. Returns exit code, stdout, and stderr (each may be truncated).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {"type": "string"},
				"timeout_s": {"type": "integer", "minimum": 1, "maximum": 300}
			},
			"required": ["command"]
		}`),
		PolicyClass: PolicyGuarded,
		Handler: func(ctx context.Context, sandboxID string, _ uuid.UUID, args json.RawMessage) (string, error) {
			var in struct {
				Command  string `json:"command"`
				TimeoutS int    `json:"timeout_s"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("agent: exec: decode args: %w", err)
			}
			timeoutS := in.TimeoutS
			if timeoutS <= 0 {
				timeoutS = defaultExecTimeoutS
			}
			if timeoutS > maxExecTimeoutS {
				timeoutS = maxExecTimeoutS
			}

			res, err := runInSandbox(ctx, client, sandboxID, in.Command, time.Duration(timeoutS)*time.Second)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("exit code: %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr), nil
		},
	}
}
