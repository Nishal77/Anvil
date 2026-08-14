package agent

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/storage"
)

func testSummarizationRouter(t *testing.T, provider llm.Provider) *llm.Router {
	t.Helper()
	router, err := llm.NewRouter(llm.Config{
		Providers: map[llm.TaskClass][]llm.Provider{llm.TaskSummarization: {provider}},
		Budget:    llm.NewInMemoryBudgetStore(150_000),
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("llm.NewRouter() error = %v", err)
	}
	return router
}

// TestCompactor_CachesSummaryAcrossTurns proves Summarize calls the
// model once for a fixed turn count, no matter how many times it's
// asked — summarization is not in the per-turn hot path.
func TestCompactor_CachesSummaryAcrossTurns(t *testing.T) {
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{Model: "fake-model", Text: "summary"})
	c := NewCompactor(testSummarizationRouter(t, provider), 0)
	jobID := uuid.New()
	turns := []storage.AgentTurn{{ToolName: "exec", Observation: "obs 1"}, {ToolName: "exec", Observation: "obs 2"}}

	for range 5 {
		summary, err := c.Summarize(context.Background(), jobID, turns)
		if err != nil {
			t.Fatalf("Summarize: %v", err)
		}
		if summary != "summary" {
			t.Errorf("summary = %q, want %q", summary, "summary")
		}
	}

	if provider.Calls() != 1 {
		t.Errorf("provider called %d times across 5 Summarize calls with unchanged turns, want 1 (cached)", provider.Calls())
	}
}

// TestCompactor_InvalidatesCacheOnNewTurns proves a changed turn count
// triggers exactly one fresh call — the cache key is turn count, not
// "ever summarized before".
func TestCompactor_InvalidatesCacheOnNewTurns(t *testing.T) {
	provider := llm.NewFakeProvider("fake").
		ScriptResponse(llm.Response{Model: "fake-model", Text: "first"}).
		ScriptResponse(llm.Response{Model: "fake-model", Text: "second"})
	c := NewCompactor(testSummarizationRouter(t, provider), 0)
	jobID := uuid.New()

	turns := []storage.AgentTurn{{ToolName: "exec", Observation: "obs 1"}}
	if _, err := c.Summarize(context.Background(), jobID, turns); err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	turns = append(turns, storage.AgentTurn{ToolName: "exec", Observation: "obs 2"})
	summary, err := c.Summarize(context.Background(), jobID, turns)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if summary != "second" {
		t.Errorf("summary = %q, want %q (fresh call after turn count changed)", summary, "second")
	}
	if provider.Calls() != 2 {
		t.Errorf("provider called %d times, want 2", provider.Calls())
	}
}
