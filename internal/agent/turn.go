package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
)

// runOneTurn is one call to the model plus the persist-then-dispatch of
// the single tool call it returns. The request is rebuilt fresh from
// persisted agent_turns every turn (via ContextBuilder), not from an
// in-memory conversation — a worker resuming this step after a crash
// must see exactly the history a live worker would, and repair_count
// accounting already lives on the step row for the same reason.
func (e *Executor) runOneTurn(ctx context.Context, job *queue.Job, steps []queue.Step, step queue.Step, sandboxID string, turnIdx int, retryHint string) (turnOutcome, error) {
	req, err := e.buildRequest(ctx, job, steps, step, retryHint)
	if err != nil {
		return turnOutcome{}, err
	}
	promptSHA := sha256.Sum256(promptDigestInput(req))

	start := time.Now()
	resp, err := e.router.Complete(ctx, job.ID, req)
	latencyMS := int(time.Since(start).Milliseconds())
	if err != nil {
		return turnOutcome{}, fmt.Errorf("agent: executor turn: llm complete: %w", err)
	}

	if err := queue.IncrementTurnCount(ctx, e.pool, step.ID); err != nil {
		e.log.ErrorContext(ctx, "increment turn count failed", slog.Any("err", err))
	}

	call, ok := firstToolCall(resp)
	if !ok {
		return turnOutcome{retryHint: "You must call exactly one tool. Call step_done if the step is already complete."}, nil
	}

	decision, reason := e.policy.Evaluate(ctx, job.ID, ToolCall{Name: call.Name, Args: call.Input, SandboxID: sandboxID})
	policyDecisionsTotal.WithLabelValues(call.Name, decision.String()).Inc()
	costUSDMicros := llm.CostUSDMicros(resp.Model, resp.Usage)

	turnID, insertErr := e.turns.InsertAgentTurn(ctx, storage.AgentTurn{
		JobID: job.ID, StepID: step.ID, TurnIdx: turnIdx, Role: "executor",
		Model: resp.Model, Provider: providerFromModel(resp.Model),
		PromptSHA256:   promptSHA[:],
		ToolName:       call.Name,
		ToolArgs:       call.Input,
		PolicyDecision: decision.String(),
		PolicyReason:   reason,
		TokensIn:       int(resp.Usage.InputTokens),
		TokensOut:      int(resp.Usage.OutputTokens),
		CostUSDMicros:  costUSDMicros,
		LatencyMS:      latencyMS,
	})
	if insertErr != nil {
		return turnOutcome{}, fmt.Errorf("agent: executor turn: persist turn: %w", insertErr)
	}

	return e.resolveTurnOutcome(ctx, step, sandboxID, turnID, call, decision, reason, resp.Usage, costUSDMicros, latencyMS)
}

// resolveTurnOutcome dispatches call, persists its result, and decides
// what this turn means for the loop: step_done ends it, an exec
// failure starts a repair, anything else just continues. Split out of
// runOneTurn to keep that function's branching within CLAUDE.md's
// cyclomatic-complexity limit.
func (e *Executor) resolveTurnOutcome(ctx context.Context, step queue.Step, sandboxID string, turnID uuid.UUID, call llm.ToolCall, decision PolicyDecision, reason string, usage llm.Usage, costUSDMicros int64, latencyMS int) (turnOutcome, error) {
	observation, execErr := e.dispatch(ctx, decision, reason, step.JobID, step.ID, sandboxID, call)
	truncated := truncateObservation(observation, e.maxObsLen)

	execErrStr := ""
	if execErr != nil {
		execErrStr = execErr.Error()
	}
	if updateErr := e.turns.UpdateAgentTurnResult(ctx, turnID, truncated, int(usage.InputTokens), int(usage.OutputTokens), costUSDMicros, latencyMS, execErrStr); updateErr != nil {
		e.log.ErrorContext(ctx, "update agent turn result failed", slog.String("turn_id", turnID.String()), slog.Any("err", updateErr))
	}
	if execErr != nil {
		return turnOutcome{}, fmt.Errorf("agent: executor turn: dispatch %s: %w", call.Name, execErr)
	}

	if call.Name == stepDoneToolName && decision == Allow {
		var args stepDoneArgs
		if err := json.Unmarshal(call.Input, &args); err != nil {
			return turnOutcome{}, fmt.Errorf("agent: executor turn: decode step_done args: %w", err)
		}
		return turnOutcome{done: true, success: args.Success, summary: args.Summary}, nil
	}

	if isExecFailure(call.Name, truncated) {
		return e.handleExecFailure(ctx, step, call.Name, truncated)
	}
	return turnOutcome{}, nil
}

// handleExecFailure implements the repair loop (PRD §12.4, FR-022):
// an exec call that exited nonzero gets a diagnose-first repair prompt
// for the next turn, up to MaxRepairsPerStep attempts. repair_count is
// persisted on the step row (not held in memory) — a crash-repair-crash
// cycle must not be able to loop forever after a reclaim resets
// whatever this worker was holding.
func (e *Executor) handleExecFailure(ctx context.Context, step queue.Step, toolName, observation string) (turnOutcome, error) {
	repairCount, err := queue.IncrementRepairCount(ctx, e.pool, step.ID)
	if err != nil {
		return turnOutcome{}, fmt.Errorf("agent: increment repair count: %w", err)
	}
	repairAttemptsTotal.Inc()
	if repairCount > e.maxRepairs {
		repairCapExceededTotal.Inc()
		return turnOutcome{}, fmt.Errorf("%w: step %d after %d repairs", ErrRepairCapExceeded, step.Idx, repairCount-1)
	}
	prompt := BuildRepairPrompt(RepairInput{Step: step, ToolName: toolName, Observation: observation, Attempt: repairCount})
	return turnOutcome{retryHint: prompt}, nil
}

// isExecFailure reports whether toolName was exec and observation
// shows a nonzero exit — the repair loop's trigger condition. exec's
// Handler always formats a successful run as "exit code: 0", so this
// is a plain prefix check, not a parse.
func isExecFailure(toolName, observation string) bool {
	return toolName == "exec" && !strings.HasPrefix(observation, "exit code: 0")
}

// buildRequest assembles the executor's request for this turn via
// ContextBuilder: the plan, the current step, and the last
// verbatimTurnCount turns of this step's own history read fresh from
// agent_turns, with anything older folded into one cached summary
// (Compactor). retryHint, if non-empty, is this turn's nudge — either
// "call exactly one tool" or a repair prompt — appended to tier 3; it
// is never persisted, since the turn it responds to already is.
func (e *Executor) buildRequest(ctx context.Context, job *queue.Job, steps []queue.Step, step queue.Step, retryHint string) (llm.Request, error) {
	all, err := e.turns.ListAgentTurns(ctx, job.ID)
	if err != nil {
		return llm.Request{}, fmt.Errorf("agent: list agent turns: %w", err)
	}
	stepTurns := make([]storage.AgentTurn, 0, len(all))
	for _, t := range all {
		if t.StepID == step.ID && t.ToolName != "" {
			stepTurns = append(stepTurns, t)
		}
	}

	var olderSummary string
	if len(stepTurns) > verbatimTurnCount {
		older := stepTurns[:len(stepTurns)-verbatimTurnCount]
		olderSummary, err = e.compact.Summarize(ctx, job.ID, older)
		if err != nil {
			return llm.Request{}, fmt.Errorf("agent: summarize older turns: %w", err)
		}
	}

	stepForBuild := step
	if retryHint != "" {
		stepForBuild.Description = step.Description + "\n\n" + retryHint
	}

	req, _ := e.context.Build(BuildInput{
		Job:          job,
		Steps:        steps,
		Step:         stepForBuild,
		RecentTurns:  reverseTurns(stepTurns),
		OlderSummary: olderSummary,
		Tools:        e.registry.ProviderTools(),
	})
	return req, nil
}

func reverseTurns(turns []storage.AgentTurn) []storage.AgentTurn {
	out := make([]storage.AgentTurn, len(turns))
	for i, t := range turns {
		out[len(turns)-1-i] = t
	}
	return out
}

// dispatch returns the tool call's observation text. A Deny never
// touches the tool's Handler — the observation is the deny reason, a
// normal, correctable turn result. An Allow runs the tool through
// callIdempotent, so a step re-run after a crash reuses the prior
// attempt's result for any call it already made instead of repeating
// a side effect.
func (e *Executor) dispatch(ctx context.Context, decision PolicyDecision, reason string, jobID, stepID uuid.UUID, sandboxID string, call llm.ToolCall) (string, error) {
	if decision != Allow {
		return fmt.Sprintf("denied: %s", reason), nil
	}

	tool, ok := e.registry.Lookup(call.Name)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrToolNotRegistered, call.Name) // unreachable: policy already checked this
	}

	result, err := callIdempotent(ctx, e.idem, jobID, stepID, call.Name, call.Input, func() (json.RawMessage, error) {
		obs, handlerErr := tool.Handler(ctx, sandboxID, call.Input)
		if handlerErr != nil {
			return nil, handlerErr
		}
		return json.Marshal(obs)
	})
	if err != nil {
		return "", err
	}

	var observation string
	if err := json.Unmarshal(result, &observation); err != nil {
		return "", fmt.Errorf("agent: decode cached observation: %w", err)
	}
	return observation, nil
}

// publish encodes data and sends it as one event of type typ for
// jobID. A failure here is logged, not returned — losing a log line
// or status marker from the live stream is not a reason to fail the
// job; the job's real state lives in the jobs and steps tables.
func (e *Executor) publish(ctx context.Context, jobID uuid.UUID, typ string, data map[string]any) {
	payload, err := json.Marshal(data)
	if err != nil {
		e.log.ErrorContext(ctx, "encode event payload failed", slog.String("type", typ), slog.Any("err", err))
		return
	}
	if err := e.pub.Publish(ctx, jobID, storage.EventType(typ), payload); err != nil {
		e.log.ErrorContext(ctx, "publish event failed", slog.String("type", typ), slog.Any("err", err))
	}
}

// executorSystemPrompt is deliberately unrefined beyond what the
// context builder and repair loop need — full prompt tuning happens
// against real benchmark results (task 7.7), not in the abstract.
const executorSystemPrompt = "You are a coding agent executing one step of a plan inside a Linux sandbox at /workspace. Use the provided tools to accomplish the step. Call exactly one tool per turn. When the step is complete, call step_done."

func firstToolCall(resp llm.Response) (llm.ToolCall, bool) {
	if len(resp.ToolCalls) == 0 {
		return llm.ToolCall{}, false
	}
	return resp.ToolCalls[0], true
}

// promptDigestInput builds the byte sequence prompt_sha256 hashes —
// system prompt plus every message's role and content, so a hash
// collision would require the same conversation verbatim, not just
// the same tail message.
func promptDigestInput(req llm.Request) []byte {
	var b strings.Builder
	b.WriteString(req.System)
	for _, m := range req.Messages {
		b.WriteString(string(m.Role))
		b.WriteString(m.Content)
		b.WriteString(m.ToolResult)
	}
	return []byte(b.String())
}

// providerFromModel derives agent_turns.provider from the response's
// model identifier. llm.Response carries only Model, not a separate
// provider field — Anvil's provider ladder has exactly two providers
// today, so a prefix match is sufficient without adding a field to a
// Week 5 type for this alone.
func providerFromModel(model string) string {
	switch {
	case strings.HasPrefix(model, "gemini"):
		return "gemini"
	case strings.HasPrefix(model, "claude"):
		return "anthropic"
	default:
		return "unknown"
	}
}
