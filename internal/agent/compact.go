package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/storage"
)

const defaultMaxSummaryTokens = 500 // PRD §12.3 tier 5's budget

// Compactor summarizes turns 4..N into a bounded summary.
//
// Summarization is NOT called per turn. It runs once when the window
// first overflows, the result is cached against the turn count it
// covers, and it is reused until the window overflows again — calling
// it every turn would double per-turn latency for no benefit.
type Compactor struct {
	router           *llm.Router
	maxSummaryTokens int

	mu    sync.Mutex
	cache map[uuid.UUID]cachedSummary
}

type cachedSummary struct {
	turnCount int // number of turns the cached summary covers
	summary   string
}

// NewCompactor constructs a Compactor. A maxSummaryTokens <= 0 defaults
// to 500 (PRD §12.3).
func NewCompactor(router *llm.Router, maxSummaryTokens int) *Compactor {
	if maxSummaryTokens <= 0 {
		maxSummaryTokens = defaultMaxSummaryTokens
	}
	return &Compactor{router: router, maxSummaryTokens: maxSummaryTokens, cache: make(map[uuid.UUID]cachedSummary)}
}

// Summarize returns a bounded summary of turns older than the verbatim
// tail for jobID. If a cached summary already covers exactly this many
// turns, it is returned without calling the model again — the cache
// key is turn count, so any new turn since the last summarization
// invalidates it and triggers exactly one fresh call.
func (c *Compactor) Summarize(ctx context.Context, jobID uuid.UUID, turns []storage.AgentTurn) (string, error) {
	if len(turns) == 0 {
		return "", nil
	}

	c.mu.Lock()
	cached, ok := c.cache[jobID]
	c.mu.Unlock()
	if ok && cached.turnCount == len(turns) {
		return cached.summary, nil
	}

	summary, err := c.summarize(ctx, jobID, turns)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	c.cache[jobID] = cachedSummary{turnCount: len(turns), summary: summary}
	c.mu.Unlock()
	return summary, nil
}

func (c *Compactor) summarize(ctx context.Context, jobID uuid.UUID, turns []storage.AgentTurn) (string, error) {
	var transcript strings.Builder
	for _, t := range turns {
		fmt.Fprintf(&transcript, "- called %s: %s\n", t.ToolName, truncateObservation(t.Observation, 500))
	}

	req := llm.Request{
		TaskClass:       llm.TaskSummarization,
		System:          "Summarize this tool-call history into a concise account of what has been tried and learned so far. Keep it under 500 tokens. Do not editorialize; state facts and outcomes only.",
		Messages:        []llm.Message{{Role: llm.RoleUser, Content: transcript.String()}},
		MaxOutputTokens: c.maxSummaryTokens,
	}

	resp, err := c.router.Complete(ctx, jobID, req)
	if err != nil {
		return "", fmt.Errorf("agent: summarize turns for job %s: %w", jobID, err)
	}
	return resp.Text, nil
}
