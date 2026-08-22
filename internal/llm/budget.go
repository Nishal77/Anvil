package llm

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"
)

// JobBudget is the subset of a job's budget state Router needs for the
// per-job check (FR-033): the ceiling, and what prior calls in this
// job already spent.
type JobBudget struct {
	TokenBudget int64
	TokensUsed  int64
}

// BudgetStore persists the running per-job token/cost totals — the
// post-call write that keeps accounting honest across retries and
// process restarts. Declared here (consumer side, CODE-STANDARDS
// §3.1): llm may not import storage (CLAUDE.md PK5), so the real
// implementation — backed by the existing jobs.token_budget /
// tokens_used / cost_usd_micros columns, no new migration — is wired
// in wherever the executor calls Router (Week 6).
type BudgetStore interface {
	GetJobBudget(ctx context.Context, jobID uuid.UUID) (JobBudget, error)
	AddJobUsage(ctx context.Context, jobID uuid.UUID, tokens, costUSDMicros int64) error
}

// InMemoryBudgetStore is a process-local BudgetStore: every job starts
// with defaultTokenBudget and nothing spent. It is not durable — a
// restart loses all accounting — so it is only appropriate where that
// is acceptable: tests, and the benchmark harness (which has no
// executor/job row to persist against until Week 6).
type InMemoryBudgetStore struct {
	defaultTokenBudget int64

	mu   sync.Mutex
	jobs map[uuid.UUID]JobBudget
}

// NewInMemoryBudgetStore constructs an InMemoryBudgetStore that grants
// every unseen job defaultTokenBudget tokens.
func NewInMemoryBudgetStore(defaultTokenBudget int64) *InMemoryBudgetStore {
	return &InMemoryBudgetStore{defaultTokenBudget: defaultTokenBudget, jobs: make(map[uuid.UUID]JobBudget)}
}

func (s *InMemoryBudgetStore) GetJobBudget(_ context.Context, jobID uuid.UUID) (JobBudget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.jobs[jobID]
	if !ok {
		b = JobBudget{TokenBudget: s.defaultTokenBudget}
		s.jobs[jobID] = b
	}
	return b, nil
}

func (s *InMemoryBudgetStore) AddJobUsage(_ context.Context, jobID uuid.UUID, tokens, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.jobs[jobID]
	if b.TokenBudget == 0 {
		b.TokenBudget = s.defaultTokenBudget
	}
	b.TokensUsed += tokens
	s.jobs[jobID] = b
	return nil
}

// SpendReader reports how much of the monthly USD cap has been spent
// so far (FR-034, NFR-011) — SUM(cost_usd_micros) over jobs created
// this month. Declared here for the same reason as BudgetStore.
type SpendReader interface {
	MonthSpendUSDMicros(ctx context.Context) (int64, error)
}

// GlobalCap enforces ANVIL_MONTHLY_USD_CAP: warns once per period at
// 80% spent (a metric, not an error) and rejects at 100% with
// ErrGlobalCapExceeded.
type GlobalCap struct {
	reader       SpendReader
	capUSDMicros int64
	log          *slog.Logger

	mu     sync.Mutex
	warned bool
}

// NewGlobalCap constructs a GlobalCap. capUSDMicros of 0 disables the
// cap (every call passes) — used in tests and when
// ANVIL_MONTHLY_USD_CAP is unset.
func NewGlobalCap(reader SpendReader, capUSDMicros int64, log *slog.Logger) *GlobalCap {
	return &GlobalCap{reader: reader, capUSDMicros: capUSDMicros, log: log}
}

// Check returns ErrGlobalCapExceeded once the monthly cap is reached.
// Called once per Router.Complete, before any provider is touched.
func (g *GlobalCap) Check(ctx context.Context) error {
	if g == nil || g.capUSDMicros <= 0 {
		return nil
	}
	spent, err := g.reader.MonthSpendUSDMicros(ctx)
	if err != nil {
		return fmt.Errorf("llm: check global cap: %w", err)
	}
	remaining := g.capUSDMicros - spent
	if remaining < 0 {
		remaining = 0
	}
	llmBudgetRemainingUSD.Set(float64(remaining) / 1_000_000)
	if spent >= g.capUSDMicros {
		return ErrGlobalCapExceeded
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.warned && spent*100 >= g.capUSDMicros*80 {
		g.warned = true
		llmGlobalCapWarnings.Inc()
		g.log.Warn("monthly LLM spend at or above 80% of cap",
			"component", "llm", "spent_usd_micros", spent, "cap_usd_micros", g.capUSDMicros)
	}
	return nil
}

// estimateHeadroom multiplies the rough len(text)/4 token estimate so
// the pre-call check errs toward over-, not under-, estimating: a
// false "budget exceeded" is a retried job, an under-estimate that
// lets a call through is a blown budget.
const estimateHeadroom = 1.3

// estimateTokens is a rough pre-check ceiling, NOT an accurate
// tokenizer. len(text)/4 is a commonly-cited approximation for
// English text; it exists only to catch a wildly oversized request
// before it reaches the provider. The real number comes from
// Response.Usage after the call (the AFTER half of FR-033).
func estimateTokens(req Request) int64 {
	var chars int
	chars += len(req.System)
	for _, m := range req.Messages {
		chars += len(m.Content) + len(m.ToolResult)
	}
	for _, t := range req.Tools {
		chars += len(t.Description) + len(t.InputSchema)
	}
	estimate := float64(chars)/4 + float64(req.MaxOutputTokens)
	return int64(estimate * estimateHeadroom)
}
