package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// stepDoneToolName is checked directly by Executor's turn loop to
// decide when a step ends — the loop needs the parsed Summary/Success
// values structurally, not just this Handler's observation string.
const stepDoneToolName = "step_done"

// stepDoneArgs is step_done's argument shape, decoded by both the
// tool Handler (for its own observation) and Executor (to learn the
// outcome).
type stepDoneArgs struct {
	Summary string `json:"summary"`
	Success bool   `json:"success"`
}

// NewStepDoneTool returns the SAFE step_done tool: the model's
// declaration that the current step is finished. It has no sandbox
// side effect — Executor.RunStep is what actually ends the turn loop
// on seeing this call.
func NewStepDoneTool() Tool {
	return Tool{
		Name:        stepDoneToolName,
		Description: "Declare the current step finished, successfully or not, with a one-line summary.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"summary": {"type": "string"},
				"success": {"type": "boolean"}
			},
			"required": ["summary", "success"]
		}`),
		PolicyClass: PolicySafe,
		Handler: func(_ context.Context, _ string, args json.RawMessage) (string, error) {
			var in stepDoneArgs
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("agent: step_done: decode args: %w", err)
			}
			return fmt.Sprintf("step marked done (success=%t): %s", in.Success, in.Summary), nil
		},
	}
}
