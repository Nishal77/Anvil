package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// GetJobTokenBudget returns jobID's token_budget and tokens_used
// (migration 002). Primitive-typed rather than returning a package
// type from internal/llm: storage may not import llm (CLAUDE.md PK5),
// so the llm.BudgetStore-shaped adapter lives in the caller that is
// allowed to depend on both (internal/agent, or the wiring in
// cmd/anvil/main.go).
func (s *Store) GetJobTokenBudget(ctx context.Context, jobID uuid.UUID) (tokenBudget, tokensUsed int64, err error) {
	const q = `SELECT token_budget, tokens_used FROM jobs WHERE id = $1`
	if err := s.pool.QueryRow(ctx, q, jobID).Scan(&tokenBudget, &tokensUsed); err != nil {
		return 0, 0, fmt.Errorf("get job token budget %s: %w", jobID, err)
	}
	return tokenBudget, tokensUsed, nil
}

// AddJobTokenUsage adds tokens and costUSDMicros to jobID's running
// totals — the post-call write PRD §14.2/INV-3 requires to keep
// accounting honest across retries and restarts.
func (s *Store) AddJobTokenUsage(ctx context.Context, jobID uuid.UUID, tokens, costUSDMicros int64) error {
	const q = `
		UPDATE jobs SET
			tokens_used = tokens_used + $2,
			cost_usd_micros = cost_usd_micros + $3,
			updated_at = now()
		WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, jobID, tokens, costUSDMicros); err != nil {
		return fmt.Errorf("add job token usage %s: %w", jobID, err)
	}
	return nil
}

// MonthSpendUSDMicros sums cost_usd_micros for jobs created since the
// start of the current calendar month — the SpendReader the global
// USD cap (FR-034) reads from. Reuses the existing jobs.cost_usd_micros
// column; no dedicated ledger table.
func (s *Store) MonthSpendUSDMicros(ctx context.Context) (int64, error) {
	const q = `
		SELECT COALESCE(SUM(cost_usd_micros), 0)
		FROM jobs
		WHERE created_at >= date_trunc('month', now())`
	var total int64
	if err := s.pool.QueryRow(ctx, q).Scan(&total); err != nil {
		return 0, fmt.Errorf("month spend usd micros: %w", err)
	}
	return total, nil
}
