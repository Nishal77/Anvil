package agent

import (
	"fmt"
	"strings"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
)

// verbatimTurnCount is tier 4 (PRD §12.3): the most recent turns kept
// in full, unsummarized. Turns older than this are tier 5's job.
const verbatimTurnCount = 3

// tokenEstimator estimates a text's token count. A heuristic, not a
// real tokenizer — the budget it enforces has headroom built in
// (MaxOutputTokens is separate), so a rough estimate that's
// consistently in the right ballpark is enough to make dropping
// decisions correctly.
type tokenEstimator func(string) int

// defaultTokenEstimator approximates 4 characters per token — the
// commonly-cited ratio for English text and code across GPT/Claude
// tokenizers; a real tokenizer per-provider is more precision than a
// budget-enforcement heuristic needs.
func defaultTokenEstimator(s string) int {
	return (len(s) + 3) / 4
}

// BuildInput is everything ContextBuilder.Build needs to assemble one
// executor request.
type BuildInput struct {
	Job          *queue.Job
	Steps        []queue.Step        // the full plan, for tier 2's step-status summary
	Step         queue.Step          // the step this request executes, tier 3
	RecentTurns  []storage.AgentTurn // most recent first; only the tail feeding tiers 4-5 need be passed
	OlderSummary string              // tier 5, from Compactor.Summarize; empty if nothing to summarize yet
	FileTree     []string            // tier 6
	TouchedFiles map[string][]byte   // tier 7
	Tools        []llm.Tool
}

// BuildStats reports how the budget was spent. DroppedTiers is
// non-empty whenever the builder had to shed content; if this never
// fires across a real run, the budget is not actually being enforced.
type BuildStats struct {
	TokensUsed   int
	DroppedTiers []int // tier numbers from PRD §12.3, dropped in reverse
}

// ContextBuilder assembles the executor's request within a token
// budget. The context window is a BUDGET, not a container: tiers 1-7
// (PRD §12.3) are added in priority order, and if the total exceeds
// maxTokens, tiers are dropped in reverse (7 -> 6 -> 5) until it fits.
// Tiers 1-4 (system prompt, plan summary, current step, last 3 turns
// verbatim) are never dropped — a request that can't fit even those
// is a planning failure, not something this builder can budget its way
// out of.
type ContextBuilder struct {
	maxTokens int
	est       tokenEstimator
}

// NewContextBuilder constructs a ContextBuilder with budget maxTokens.
// A nil est defaults to defaultTokenEstimator.
func NewContextBuilder(maxTokens int, est tokenEstimator) *ContextBuilder {
	if est == nil {
		est = defaultTokenEstimator
	}
	return &ContextBuilder{maxTokens: maxTokens, est: est}
}

// Build assembles the executor request within the token budget.
func (b *ContextBuilder) Build(in BuildInput) (llm.Request, BuildStats) {
	tier2 := formatPlanTier(in.Job, in.Steps)
	tier3 := formatStepTier(in.Step)
	tier5 := in.OlderSummary
	tier6 := formatFileTreeTier(in.FileTree)
	tier7 := formatTouchedFilesTier(in.TouchedFiles)

	included := map[int]bool{2: true, 3: true, 4: true, 5: tier5 != "", 6: tier6 != "", 7: tier7 != ""}
	tierText := map[int]string{2: tier2, 3: tier3, 5: tier5, 6: tier6, 7: tier7}

	var stats BuildStats
	for _, tier := range []int{7, 6, 5} {
		if b.total(in, included, tierText) <= b.maxTokens {
			break
		}
		if !included[tier] {
			continue
		}
		included[tier] = false
		stats.DroppedTiers = append(stats.DroppedTiers, tier)
		contextPressureTotal.Inc()
	}

	req := llm.Request{
		TaskClass: llm.TaskExecution,
		System:    executorSystemPrompt,
		Tools:     in.Tools,
		Messages:  b.assembleMessages(in, included, tierText),
	}
	stats.TokensUsed = b.total(in, included, tierText) + b.est(executorSystemPrompt)
	return req, stats
}

// total sums the estimated token cost of every currently-included
// tier, plus the verbatim tail (tier 4) — the only tier whose size
// depends on data not precomputed into tierText.
func (b *ContextBuilder) total(in BuildInput, included map[int]bool, tierText map[int]string) int {
	sum := 0
	for tier, text := range tierText {
		if included[tier] {
			sum += b.est(text)
		}
	}
	for _, t := range verbatimTail(in.RecentTurns) {
		sum += b.est(t.ToolName) + b.est(string(t.ToolArgs)) + b.est(t.Observation)
	}
	return sum
}

// assembleMessages builds the final message list: one context message
// carrying whichever of tiers 2/3/5/6/7 survived the budget, followed
// by the verbatim replay of tier 4's turns.
func (b *ContextBuilder) assembleMessages(in BuildInput, included map[int]bool, tierText map[int]string) []llm.Message {
	var context strings.Builder
	for _, tier := range []int{2, 3, 5, 6, 7} {
		if included[tier] && tierText[tier] != "" {
			context.WriteString(tierText[tier])
			context.WriteString("\n\n")
		}
	}

	messages := []llm.Message{{Role: llm.RoleUser, Content: strings.TrimSpace(context.String())}}
	for _, t := range verbatimTail(in.RecentTurns) {
		if t.ToolName == "" {
			continue
		}
		messages = append(messages,
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: t.ToolName, Input: t.ToolArgs}}},
			llm.Message{Role: llm.RoleTool, ToolCallID: t.ToolName, ToolResult: t.Observation},
		)
	}
	return messages
}

// verbatimTail returns the oldest-first slice of the most recent
// verbatimTurnCount turns from turns (which arrives most-recent-first),
// so replay lands in the order the conversation actually happened.
func verbatimTail(turns []storage.AgentTurn) []storage.AgentTurn {
	n := min(len(turns), verbatimTurnCount)
	tail := make([]storage.AgentTurn, n)
	for i := range n {
		tail[i] = turns[n-1-i]
	}
	return tail
}

func formatPlanTier(job *queue.Job, steps []queue.Step) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Job summary: %s\n", job.PlanSummary)
	b.WriteString("Plan:\n")
	for _, s := range steps {
		fmt.Fprintf(&b, "  [%d] %s — %s\n", s.Idx, s.Title, s.Status)
	}
	return b.String()
}

func formatStepTier(step queue.Step) string {
	return fmt.Sprintf("Current step: %s\n%s\nAcceptance: %s", step.Title, step.Description, step.Acceptance)
}

const maxFileTreeEntries = 100

func formatFileTreeTier(tree []string) string {
	if len(tree) == 0 {
		return ""
	}
	entries := tree
	if len(entries) > maxFileTreeEntries {
		entries = entries[:maxFileTreeEntries]
	}
	return "File tree:\n" + strings.Join(entries, "\n")
}

const maxTouchedFileBytes = 4096

func formatTouchedFilesTier(files map[string][]byte) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Touched files:\n")
	for path, content := range files {
		if len(content) > maxTouchedFileBytes {
			content = content[:maxTouchedFileBytes]
		}
		fmt.Fprintf(&b, "--- %s ---\n%s\n", path, content)
	}
	return b.String()
}
