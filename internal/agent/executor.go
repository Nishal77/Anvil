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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
)

// sandboxManager is everything Executor itself needs beyond the
// narrower sandboxClient (declared in sandbox.go) that tools and the
// policy engine use — Create/Destroy bracket one job's sandbox
// lifetime, Exec is inherited for the interface embedding to satisfy
// sandboxClient wherever Executor passes itself around.
type sandboxManager interface {
	sandboxClient
	Create(ctx context.Context, jobID uuid.UUID) (string, error)
	Destroy(ctx context.Context, sandboxID string) error
}

// publisher is the subset of events.Publisher the executor needs.
type publisher interface {
	Publish(ctx context.Context, jobID uuid.UUID, typ storage.EventType, payload json.RawMessage) error
}

// TurnStore persists the agent_turns audit trail (migration 004).
type TurnStore interface {
	InsertAgentTurn(ctx context.Context, t storage.AgentTurn) (uuid.UUID, error)
	UpdateAgentTurnResult(ctx context.Context, id uuid.UUID, observation string, tokensIn, tokensOut int, costUSDMicros int64, latencyMS int, execErr string) error
}

const defaultMaxTurnsPerStep = 12 // FR-021

// Config configures an Executor.
type Config struct {
	Registry  *Registry
	Policy    Policy
	Router    *llm.Router
	Sandbox   sandboxManager
	Publisher publisher
	IdemStore IdemStore
	Turns     TurnStore
	Pool      *pgxpool.Pool // steps and jobs.sandbox_id live in queue's tables, read/written directly
	Logger    *slog.Logger

	MaxTurnsPerStep     int // default 12 (FR-021)
	MaxObservationBytes int // default 8192 (FR-024)
}

func (c *Config) setDefaults() {
	if c.MaxTurnsPerStep <= 0 {
		c.MaxTurnsPerStep = defaultMaxTurnsPerStep
	}
	if c.MaxObservationBytes <= 0 {
		c.MaxObservationBytes = defaultMaxObservationBytes
	}
}

func (c Config) validate() error {
	for name, v := range map[string]bool{
		"Registry": c.Registry == nil, "Policy": c.Policy == nil, "Router": c.Router == nil,
		"Sandbox": c.Sandbox == nil, "Publisher": c.Publisher == nil, "IdemStore": c.IdemStore == nil,
		"Turns": c.Turns == nil, "Pool": c.Pool == nil, "Logger": c.Logger == nil,
	} {
		if v {
			return fmt.Errorf("agent: config: %s is required", name)
		}
	}
	return nil
}

// Executor is the Executor role of PRD §12.1: a per-step turn loop
// against an llm.Router, gated by a Policy, with every turn persisted
// to agent_turns before its tool call is dispatched.
type Executor struct {
	registry  *Registry
	policy    Policy
	router    *llm.Router
	sandbox   sandboxManager
	pub       publisher
	idem      IdemStore
	turns     TurnStore
	pool      *pgxpool.Pool
	log       *slog.Logger
	maxTurns  int
	maxObsLen int
}

// New constructs an Executor from cfg, or returns an error if cfg is
// invalid.
func New(cfg Config) (*Executor, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Executor{
		registry: cfg.Registry, policy: cfg.Policy, router: cfg.Router,
		sandbox: cfg.Sandbox, pub: cfg.Publisher, idem: cfg.IdemStore, turns: cfg.Turns,
		pool: cfg.Pool, log: cfg.Logger, maxTurns: cfg.MaxTurnsPerStep, maxObsLen: cfg.MaxObservationBytes,
	}, nil
}

// hardcodedStep is one step of the fixed plan (still correct this
// week — the planner lands in Week 7). Title and Description are the
// instruction handed to the executor turn loop; there is no longer a
// fixed Command; the model decides which tools to call to satisfy
// Description.
type hardcodedStep struct {
	Title       string
	Description string
}

var hardcodedPlan = []hardcodedStep{
	{
		Title:       "Initialize a Go module",
		Description: "Create /workspace/app and a go.mod declaring module \"app\" with go 1.23, using fs_write and exec as needed.",
	},
	{
		Title:       "Write a Go program with a passing test",
		Description: "In /workspace/app, write main.go with a Greet() function returning \"Hello, World!\" and a main() that prints it, plus main_test.go verifying Greet(). Run `go test ./...` with exec and confirm it passes before calling step_done.",
	},
	{
		Title:       "Confirm the build compiles",
		Description: "Run `go build ./...` in /workspace/app with exec and confirm it exits 0 before calling step_done.",
	},
}

// RunStep matches queue.Dispatcher.Config.RunStep's signature exactly,
// so cmd/anvil wires it in directly. It runs every step of
// hardcodedPlan against one sandbox, in order, skipping any step
// already SUCCEEDED — a step's status in Postgres is the only thing
// RunStep trusts about what already happened, which is what makes
// resuming after a crash safe.
func (e *Executor) RunStep(ctx context.Context, job *queue.Job) error {
	sandboxID, err := e.ensureSandbox(ctx, job)
	if err != nil {
		return err
	}

	runErr := e.runAllSteps(ctx, job.ID, sandboxID)

	// See internal/agent's predecessor (internal/executor) for why this
	// check exists: a cancelled ctx means the process is shutting down
	// and another worker will resume this same sandbox, so it must not
	// be destroyed out from under that resumption.
	if ctx.Err() == nil {
		if err := e.sandbox.Destroy(context.WithoutCancel(ctx), sandboxID); err != nil {
			e.log.ErrorContext(ctx, "destroy sandbox failed", slog.String("sandbox_id", sandboxID), slog.Any("err", err))
		}
	}
	return runErr
}

func (e *Executor) ensureSandbox(ctx context.Context, job *queue.Job) (string, error) {
	if job.SandboxID != "" {
		return job.SandboxID, nil
	}
	sandboxID, err := e.sandbox.Create(ctx, job.ID)
	if err != nil {
		return "", fmt.Errorf("agent: create sandbox: %w", err)
	}
	if err := queue.SetJobSandboxID(ctx, e.pool, job.ID, sandboxID); err != nil {
		return "", fmt.Errorf("agent: persist sandbox id: %w", err)
	}
	return sandboxID, nil
}

func (e *Executor) runAllSteps(ctx context.Context, jobID uuid.UUID, sandboxID string) error {
	for idx, hs := range hardcodedPlan {
		step, err := queue.EnsureStep(ctx, e.pool, jobID, idx, hs.Title, hs.Description)
		if err != nil {
			return fmt.Errorf("agent: ensure step %d: %w", idx, err)
		}
		if step.Status == queue.StepSucceeded {
			continue
		}
		if err := e.runOneStep(ctx, jobID, sandboxID, step, hs); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) runOneStep(ctx context.Context, jobID uuid.UUID, sandboxID string, step queue.Step, hs hardcodedStep) error {
	if err := queue.StartStep(ctx, e.pool, step.ID); err != nil {
		return fmt.Errorf("agent: start step %d: %w", step.Idx, err)
	}
	e.publish(ctx, jobID, "step_started", map[string]any{"step_idx": step.Idx, "title": hs.Title})

	success, summary, err := e.runTurnLoop(ctx, jobID, step, sandboxID, hs)
	if err != nil {
		if finishErr := queue.FinishStep(ctx, e.pool, step.ID, queue.StepFailed, err.Error()); finishErr != nil {
			return fmt.Errorf("agent: finish step %d: %w", step.Idx, finishErr)
		}
		e.publish(ctx, jobID, "step_finished", map[string]any{"step_idx": step.Idx, "status": "FAILED"})
		return fmt.Errorf("agent: step %d (%s): %w", step.Idx, hs.Title, err)
	}
	if !success {
		if finishErr := queue.FinishStep(ctx, e.pool, step.ID, queue.StepFailed, summary); finishErr != nil {
			return fmt.Errorf("agent: finish step %d: %w", step.Idx, finishErr)
		}
		e.publish(ctx, jobID, "step_finished", map[string]any{"step_idx": step.Idx, "status": "FAILED"})
		return fmt.Errorf("agent: step %d (%s): model reported failure: %s", step.Idx, hs.Title, summary)
	}

	if err := queue.FinishStep(ctx, e.pool, step.ID, queue.StepSucceeded, ""); err != nil {
		return fmt.Errorf("agent: finish step %d: %w", step.Idx, err)
	}
	e.publish(ctx, jobID, "step_finished", map[string]any{"step_idx": step.Idx, "status": "SUCCEEDED"})
	return nil
}

// runTurnLoop drives the executor turn loop for one step: call the
// model, persist-then-dispatch its one tool call, feed the observation
// back, repeat until step_done or MaxTurnsPerStep is reached. A
// repair turn (a turn following a failed tool dispatch) counts against
// MaxTurnsPerStep like any other turn — there is no separate repair
// budget yet (that lands in Week 7's repair loop, PRD §12.4); this
// loop's turn cap is the hard ceiling FR-021 requires regardless.
func (e *Executor) runTurnLoop(ctx context.Context, jobID uuid.UUID, step queue.Step, sandboxID string, hs hardcodedStep) (success bool, summary string, err error) {
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: fmt.Sprintf("Step: %s\n\n%s\n\nWhen finished, call step_done.", hs.Title, hs.Description)},
	}

	for turnIdx := 0; turnIdx < e.maxTurns; turnIdx++ {
		stepTurnsTotal.Inc()

		outcome, loopErr := e.runOneTurn(ctx, jobID, step, sandboxID, turnIdx, messages)
		if loopErr != nil {
			return false, "", loopErr
		}
		messages = outcome.messages
		if outcome.done {
			return outcome.success, outcome.summary, nil
		}
	}
	return false, "", fmt.Errorf("%w: step %d after %d turns", ErrStepTurnLimitExceeded, step.Idx, e.maxTurns)
}

type turnOutcome struct {
	messages []llm.Message
	done     bool
	success  bool
	summary  string
}

// runOneTurn is one call to the model plus the persist-then-dispatch
// of the single tool call it returns. Split out of runTurnLoop to keep
// the loop's own cyclomatic complexity low (CLAUDE.md §5.1).
func (e *Executor) runOneTurn(ctx context.Context, jobID uuid.UUID, step queue.Step, sandboxID string, turnIdx int, messages []llm.Message) (turnOutcome, error) {
	req := llm.Request{
		TaskClass: llm.TaskExecution,
		System:    executorSystemPrompt,
		Messages:  messages,
		Tools:     e.registry.ProviderTools(),
	}
	promptSHA := sha256.Sum256(promptDigestInput(req))

	start := time.Now()
	resp, err := e.router.Complete(ctx, jobID, req)
	latencyMS := int(time.Since(start).Milliseconds())
	if err != nil {
		return turnOutcome{}, fmt.Errorf("agent: executor turn: llm complete: %w", err)
	}

	call, ok := firstToolCall(resp)
	if !ok {
		messages = append(messages,
			llm.Message{Role: llm.RoleAssistant, Content: resp.Text},
			llm.Message{Role: llm.RoleUser, Content: "You must call exactly one tool. Call step_done if the step is already complete."},
		)
		return turnOutcome{messages: messages}, nil
	}

	decision, reason := e.policy.Evaluate(ctx, jobID, ToolCall{Name: call.Name, Args: call.Input, SandboxID: sandboxID})
	policyDecisionsTotal.WithLabelValues(call.Name, decision.String()).Inc()
	costUSDMicros := llm.CostUSDMicros(resp.Model, resp.Usage)

	turnID, insertErr := e.turns.InsertAgentTurn(ctx, storage.AgentTurn{
		JobID: jobID, StepID: step.ID, TurnIdx: turnIdx, Role: "executor",
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

	observation, execErr := e.dispatch(ctx, decision, reason, jobID, step.ID, sandboxID, call)
	truncated := truncateObservation(observation, e.maxObsLen)

	execErrStr := ""
	if execErr != nil {
		execErrStr = execErr.Error()
	}
	if updateErr := e.turns.UpdateAgentTurnResult(ctx, turnID, truncated, int(resp.Usage.InputTokens), int(resp.Usage.OutputTokens), costUSDMicros, latencyMS, execErrStr); updateErr != nil {
		e.log.ErrorContext(ctx, "update agent turn result failed", slog.String("turn_id", turnID.String()), slog.Any("err", updateErr))
	}
	if execErr != nil {
		return turnOutcome{}, fmt.Errorf("agent: executor turn: dispatch %s: %w", call.Name, execErr)
	}

	messages = append(messages,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}},
		llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, ToolResult: truncated},
	)

	if call.Name == stepDoneToolName && decision == Allow {
		var args stepDoneArgs
		if err := json.Unmarshal(call.Input, &args); err != nil {
			return turnOutcome{}, fmt.Errorf("agent: executor turn: decode step_done args: %w", err)
		}
		return turnOutcome{done: true, success: args.Success, summary: args.Summary}, nil
	}
	return turnOutcome{messages: messages}, nil
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

// executorSystemPrompt is deliberately unrefined — prompt tuning is
// Week 7 (context builder, PRD §12.3). This is enough to run the loop
// end to end and produce a real baseline.
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
