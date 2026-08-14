package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// AgentTurn is a row of agent_turns — the full audit trail of one
// model interaction (PRD §10, FR-025). Prompt bodies never appear
// here; PromptSHA256 is what CLAUDE.md invariant I-3 requires instead.
type AgentTurn struct {
	ID             uuid.UUID
	JobID          uuid.UUID
	StepID         uuid.UUID
	TurnIdx        int
	Role           string
	Model          string
	Provider       string
	PromptSHA256   []byte
	ToolName       string
	ToolArgs       json.RawMessage
	PolicyDecision string
	PolicyReason   string
	Observation    string
	TokensIn       int
	TokensOut      int
	CostUSDMicros  int64
	LatencyMS      int
	Error          string
	CreatedAt      time.Time
}

// InsertAgentTurn persists one turn. Callers insert BEFORE dispatching
// the tool call (PRD §16.3, §14.2): if the process dies mid-call, the
// row already shows what was attempted and with what policy decision.
func (s *Store) InsertAgentTurn(ctx context.Context, t AgentTurn) (uuid.UUID, error) {
	const q = `
		INSERT INTO agent_turns (
			job_id, step_id, turn_idx, role, model, provider,
			prompt_sha256, tool_name, tool_args,
			policy_decision, policy_reason, observation,
			tokens_in, tokens_out, cost_usd_micros, latency_ms, error
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		RETURNING id`

	var id uuid.UUID
	err := s.pool.QueryRow(ctx, q,
		t.JobID, t.StepID, t.TurnIdx, t.Role, t.Model, t.Provider,
		t.PromptSHA256, nullString(t.ToolName), nullRawMessage(t.ToolArgs),
		t.PolicyDecision, nullString(t.PolicyReason), nullString(t.Observation),
		t.TokensIn, t.TokensOut, t.CostUSDMicros, nullInt(t.LatencyMS), nullString(t.Error),
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert agent turn: %w", err)
	}
	return id, nil
}

// UpdateAgentTurnResult fills in a previously-inserted turn's
// execution outcome (observation, tokens, cost, latency, error) —
// the second half of persist-then-execute.
func (s *Store) UpdateAgentTurnResult(ctx context.Context, id uuid.UUID, observation string, tokensIn, tokensOut int, costUSDMicros int64, latencyMS int, execErr string) error {
	const q = `
		UPDATE agent_turns SET
			observation = $2, tokens_in = $3, tokens_out = $4,
			cost_usd_micros = $5, latency_ms = $6, error = $7
		WHERE id = $1`

	if _, err := s.pool.Exec(ctx, q, id, nullString(observation), tokensIn, tokensOut, costUSDMicros, nullInt(latencyMS), nullString(execErr)); err != nil {
		return fmt.Errorf("update agent turn %s: %w", id, err)
	}
	return nil
}

// ListAgentTurns returns jobID's turns in turn_idx order — the audit
// query behind "every tool call appears in agent_turns with its
// policy decision".
func (s *Store) ListAgentTurns(ctx context.Context, jobID uuid.UUID) ([]AgentTurn, error) {
	const q = `
		SELECT id, job_id, step_id, turn_idx, role, model, provider,
		       prompt_sha256, COALESCE(tool_name, ''), tool_args,
		       policy_decision, COALESCE(policy_reason, ''), COALESCE(observation, ''),
		       tokens_in, tokens_out, cost_usd_micros, COALESCE(latency_ms, 0),
		       COALESCE(error, ''), created_at
		FROM agent_turns
		WHERE job_id = $1
		ORDER BY turn_idx
		LIMIT 1000`

	rows, err := s.pool.Query(ctx, q, jobID)
	if err != nil {
		return nil, fmt.Errorf("list agent turns for job %s: %w", jobID, err)
	}
	defer rows.Close()

	var turns []AgentTurn
	for rows.Next() {
		var t AgentTurn
		var stepID *uuid.UUID
		if err := rows.Scan(
			&t.ID, &t.JobID, &stepID, &t.TurnIdx, &t.Role, &t.Model, &t.Provider,
			&t.PromptSHA256, &t.ToolName, &t.ToolArgs,
			&t.PolicyDecision, &t.PolicyReason, &t.Observation,
			&t.TokensIn, &t.TokensOut, &t.CostUSDMicros, &t.LatencyMS,
			&t.Error, &t.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("list agent turns for job %s: scan: %w", jobID, err)
		}
		if stepID != nil {
			t.StepID = *stepID
		}
		turns = append(turns, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list agent turns for job %s: %w", jobID, err)
	}
	return turns, nil
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullInt(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func nullRawMessage(m json.RawMessage) *json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	return &m
}
