package agent

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
)

func testBuildInput(recentTurns []storage.AgentTurn, fileTree []string) BuildInput {
	jobID := uuid.New()
	return BuildInput{
		Job:         &queue.Job{ID: jobID, PlanSummary: "build a thing"},
		Steps:       []queue.Step{{JobID: jobID, Idx: 0, Title: "step 0", Status: queue.StepRunning}},
		Step:        queue.Step{JobID: jobID, Idx: 0, Title: "step 0", Description: "do the thing", Acceptance: "go test passes"},
		RecentTurns: recentTurns,
		FileTree:    fileTree,
	}
}

func bigTurn(toolName string) storage.AgentTurn {
	return storage.AgentTurn{ToolName: toolName, Observation: strings.Repeat("x", 2000)}
}

// TestContextBuilder_StaysWithinBudget proves 200 turns of history
// still produces a request under the configured token budget — the
// budget is enforced by dropping, not by hoping the input is small.
func TestContextBuilder_StaysWithinBudget(t *testing.T) {
	var turns []storage.AgentTurn
	for range 200 {
		turns = append(turns, bigTurn("exec"))
	}
	tree := make([]string, 500)
	for i := range tree {
		tree[i] = "/workspace/file.go"
	}

	b := NewContextBuilder(2000, nil)
	req, stats := b.Build(testBuildInput(turns, tree))

	total := b.est(req.System)
	for _, m := range req.Messages {
		total += b.est(m.Content) + b.est(m.ToolResult)
	}
	if total > 2000 {
		t.Errorf("assembled request ~%d tokens, want <= 2000 (budget)", total)
	}
	if stats.TokensUsed > 2000 {
		t.Errorf("BuildStats.TokensUsed = %d, want <= 2000", stats.TokensUsed)
	}
}

// TestContextBuilder_DropsInReverseTierOrder proves tier 7 (touched
// files) is dropped before tier 6 (file tree) when both must go.
func TestContextBuilder_DropsInReverseTierOrder(t *testing.T) {
	in := testBuildInput(nil, []string{"/workspace/main.go"})
	in.TouchedFiles = map[string][]byte{"/workspace/main.go": []byte(strings.Repeat("x", 5000))}

	// A budget that fits tiers 2-6 but not tier 7's large file content.
	b := NewContextBuilder(50, nil)
	_, stats := b.Build(in)

	if len(stats.DroppedTiers) == 0 {
		t.Fatal("DroppedTiers is empty, want at least tier 7 dropped")
	}
	if stats.DroppedTiers[0] != 7 {
		t.Errorf("first dropped tier = %d, want 7 (dropped before tier 6)", stats.DroppedTiers[0])
	}
}

// TestContextBuilder_EmitsContextPressureOnDrop proves the metric
// actually increments — a budget that's never observed to trigger a
// drop is not proven to be enforced.
func TestContextBuilder_EmitsContextPressureOnDrop(t *testing.T) {
	before := testCounterValue(t, contextPressureTotal)

	in := testBuildInput(nil, []string{"/workspace/main.go"})
	in.TouchedFiles = map[string][]byte{"/workspace/main.go": []byte(strings.Repeat("x", 5000))}
	b := NewContextBuilder(10, nil)
	b.Build(in)

	after := testCounterValue(t, contextPressureTotal)
	if after <= before {
		t.Errorf("contextPressureTotal did not increment: before=%v after=%v", before, after)
	}
}

// TestContextBuilder_ToolCallAndResultShareID proves the replayed
// assistant/tool message pair for a verbatim turn share one ID. A
// mismatch here (previously: the assistant side had no ID at all, and
// the tool side carried the tool NAME instead) is invisible to every
// test using llm.FakeProvider (which ignores request content), but a
// real provider like Anthropic hard-rejects a tool_result whose
// tool_use_id doesn't match the preceding tool_use block — a live-run
// failure that only a real API call surfaces.
func TestContextBuilder_ToolCallAndResultShareID(t *testing.T) {
	turnID := uuid.New()
	turns := []storage.AgentTurn{{ID: turnID, ToolName: "exec", Observation: "exit code: 0"}}

	b := NewContextBuilder(defaultMaxContextTokens, nil)
	req, _ := b.Build(testBuildInput(turns, nil))

	var assistantID, toolCallID string
	for _, m := range req.Messages {
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			assistantID = m.ToolCalls[0].ID
		}
		if m.Role == llm.RoleTool {
			toolCallID = m.ToolCallID
		}
	}
	if assistantID == "" {
		t.Fatal("assistant ToolCalls[0].ID is empty")
	}
	if assistantID != toolCallID {
		t.Errorf("assistant tool_use ID = %q, tool_result ToolCallID = %q — must match", assistantID, toolCallID)
	}
}

// TestContextBuilder_NoDropWithinBudget proves a small, well-within-budget
// request is left untouched — dropping is a last resort, not a default.
func TestContextBuilder_NoDropWithinBudget(t *testing.T) {
	b := NewContextBuilder(defaultMaxContextTokens, nil)
	_, stats := b.Build(testBuildInput(nil, nil))

	if len(stats.DroppedTiers) != 0 {
		t.Errorf("DroppedTiers = %v, want empty for a small request", stats.DroppedTiers)
	}
}
