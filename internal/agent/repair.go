package agent

import (
	"fmt"

	"github.com/anvil-dev/anvil/internal/queue"
)

// RepairInput describes a failed attempt for the repair prompt.
type RepairInput struct {
	Step        queue.Step
	ToolName    string
	Observation string
	Attempt     int // 1-based
}

// BuildRepairPrompt frames the failure for the model.
//
// The framing instructs the model to DIAGNOSE BEFORE ACTING. A repair
// loop that re-issues a variant of the same command is a retry loop
// with extra token cost, not a repair loop — the prompt asks explicitly
// for a diagnosis before whatever tool call comes next.
func BuildRepairPrompt(in RepairInput) string {
	return fmt.Sprintf(
		"The last tool call (%s) did not satisfy this step's acceptance criterion: %q\n\n"+
			"Observation:\n%s\n\n"+
			"This is repair attempt %d of %d. Before calling another tool, state in one sentence what actually went wrong — "+
			"not what you'll try next, what the evidence above shows is broken. Then take the smallest action that addresses "+
			"that diagnosed cause. Re-issuing a variant of the same command without a diagnosis is not a repair.",
		in.ToolName, in.Step.Acceptance, in.Observation, in.Attempt, defaultMaxRepairsPerStep,
	)
}
