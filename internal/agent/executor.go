package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
	"github.com/anvil-dev/anvil/internal/telemetry"
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
	ExportWorkspace(ctx context.Context, sandboxID string) (io.ReadCloser, error)
}

// artifactUploader is the subset of *artifact.Store the executor
// needs, declared at the consumer per CODE-STANDARDS §3.1. Optional:
// a nil Config.Artifacts skips upload entirely, for tests and any
// deployment that hasn't wired object storage.
type artifactUploader interface {
	Upload(ctx context.Context, jobID uuid.UUID, r io.Reader, size int64) (string, error)
}

// publisher is the subset of events.Publisher the executor needs.
type publisher interface {
	Publish(ctx context.Context, jobID uuid.UUID, typ storage.EventType, payload json.RawMessage) error
}

// deployer is the subset of *deploy.DockerDeployer the executor
// needs, declared at the consumer (CODE-STANDARDS §3.1). agent's
// declared dependency graph (CLAUDE.md PK5) is llm, sandbox, storage,
// events — it does not include deploy, so *deploy.DockerDeployer is
// wired in structurally, satisfying this interface without agent
// importing the deploy package at all. Wiring happens in
// cmd/anvil/main.go, which is free to import both.
type deployer interface {
	Deploy(ctx context.Context, jobID uuid.UUID, archive []byte) (previewURL string, err error)
}

// TurnStore persists the agent_turns audit trail (migration 004) and
// serves it back — ListAgentTurns is what ContextBuilder's tier 4/5
// read on every turn, rather than an in-memory conversation history
// that a crash-resumed worker would otherwise have lost.
type TurnStore interface {
	InsertAgentTurn(ctx context.Context, t storage.AgentTurn) (uuid.UUID, error)
	UpdateAgentTurnResult(ctx context.Context, id uuid.UUID, observation string, tokensIn, tokensOut int, costUSDMicros int64, latencyMS int, execErr string) error
	ListAgentTurns(ctx context.Context, jobID uuid.UUID) ([]storage.AgentTurn, error)
}

const (
	defaultMaxTurnsPerStep   = 12    // FR-021
	defaultMaxRepairsPerStep = 3     // FR-022
	defaultMaxContextTokens  = 32000 // PRD §12.3
)

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

	CancelWatcher  CancelWatcher   // nil defaults to a watcher backed by Pool
	ContextBuilder *ContextBuilder // nil defaults to NewContextBuilder(defaultMaxContextTokens, nil)
	Compactor      *Compactor      // nil defaults to NewCompactor(Router, 0)
	// Artifacts uploads a job's workspace on every terminal path
	// (ADR-012: failure preserves the artifact). nil skips upload —
	// tests and any deployment without object storage configured.
	Artifacts artifactUploader
	// Deployer builds and runs a preview deployment for a job whose
	// options.deploy was set (PRD §11.3, §13.1's DEPLOYING state).
	// nil skips deploy entirely, even for a job that requested it —
	// tests and any deployment without Docker/Caddy configured for
	// previews.
	Deployer deployer

	MaxTurnsPerStep     int // default 12 (FR-021)
	MaxObservationBytes int // default 8192 (FR-024)
	MaxRepairsPerStep   int // default 3 (FR-022)
	MaxContextTokens    int // default 32000 (PRD §12.3)
}

func (c *Config) setDefaults() {
	if c.MaxTurnsPerStep <= 0 {
		c.MaxTurnsPerStep = defaultMaxTurnsPerStep
	}
	if c.MaxObservationBytes <= 0 {
		c.MaxObservationBytes = defaultMaxObservationBytes
	}
	if c.MaxRepairsPerStep <= 0 {
		c.MaxRepairsPerStep = defaultMaxRepairsPerStep
	}
	if c.MaxContextTokens <= 0 {
		c.MaxContextTokens = defaultMaxContextTokens
	}
	if c.CancelWatcher == nil && c.Pool != nil {
		c.CancelWatcher = NewCancelWatcher(c.Pool)
	}
	if c.ContextBuilder == nil {
		c.ContextBuilder = NewContextBuilder(c.MaxContextTokens, nil)
	}
	if c.Compactor == nil && c.Router != nil {
		c.Compactor = NewCompactor(c.Router, 0)
	}
}

func (c Config) validate() error {
	for name, v := range map[string]bool{
		"Registry": c.Registry == nil, "Policy": c.Policy == nil, "Router": c.Router == nil,
		"Sandbox": c.Sandbox == nil, "Publisher": c.Publisher == nil, "IdemStore": c.IdemStore == nil,
		"Turns": c.Turns == nil, "Pool": c.Pool == nil, "Logger": c.Logger == nil,
		"CancelWatcher": c.CancelWatcher == nil, "ContextBuilder": c.ContextBuilder == nil, "Compactor": c.Compactor == nil,
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
	registry   *Registry
	policy     Policy
	router     *llm.Router
	sandbox    sandboxManager
	pub        publisher
	idem       IdemStore
	turns      TurnStore
	pool       *pgxpool.Pool
	log        *slog.Logger
	cancel     CancelWatcher
	context    *ContextBuilder
	compact    *Compactor
	artifacts  artifactUploader
	deployer   deployer
	maxTurns   int
	maxObsLen  int
	maxRepairs int
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
		pool: cfg.Pool, log: cfg.Logger, cancel: cfg.CancelWatcher, context: cfg.ContextBuilder, compact: cfg.Compactor,
		artifacts: cfg.Artifacts, deployer: cfg.Deployer,
		maxTurns: cfg.MaxTurnsPerStep, maxObsLen: cfg.MaxObservationBytes, maxRepairs: cfg.MaxRepairsPerStep,
	}, nil
}

// RunStep matches queue.Dispatcher.Config.RunStep's signature exactly,
// so cmd/anvil wires it in directly. It runs every PENDING step of
// job's plan against one sandbox, in idx order, skipping any step
// already SUCCEEDED or SKIPPED — a step's status in Postgres is the
// only thing RunStep trusts about what already happened, which is what
// makes resuming after a crash safe. The plan itself was written by
// the Planner before job ever reached QUEUED (queue.SavePlan); RunStep
// only reads it.
func (e *Executor) RunStep(ctx context.Context, job *queue.Job) error {
	sandboxID, err := e.ensureSandbox(ctx, job)
	if err != nil {
		return err
	}

	runErr := e.runAllSteps(ctx, job, sandboxID)

	if errors.Is(runErr, ErrCancelled) {
		e.uploadArtifact(context.WithoutCancel(ctx), job, sandboxID)
		e.finishCancellation(ctx, job, sandboxID)
		return nil
	}

	// See internal/agent's predecessor (internal/executor) for why this
	// check exists: a cancelled ctx means the process is shutting down
	// and another worker will resume this same sandbox, so it must not
	// be destroyed out from under that resumption.
	if ctx.Err() == nil {
		archive := e.uploadArtifact(ctx, job, sandboxID)
		if runErr == nil && job.Deploy && e.deployer != nil {
			// deployPreview performs its own RUNNING -> DEPLOYING ->
			// {SUCCEEDED, FAILED} transition (PRD §13.1) and returns
			// the outcome as runErr, exactly like finishCancellation's
			// self-transition above: dispatcher.go's runJob only
			// auto-transitions a job still sitting in the status it
			// was claimed into, so a job this branch has already
			// moved out of RUNNING is left alone.
			runErr = e.deployPreview(ctx, job, archive)
		}
		if err := e.sandbox.Destroy(context.WithoutCancel(ctx), sandboxID); err != nil {
			e.log.ErrorContext(ctx, "destroy sandbox failed", slog.String("sandbox_id", sandboxID), slog.Any("err", err))
		}
	}
	return runErr
}

// uploadArtifact exports sandboxID's /workspace and uploads it before
// the sandbox is destroyed, on every terminal path — SUCCEEDED,
// FAILED, and CANCELLED alike (ADR-012: failure preserves the
// artifact). Best-effort: an upload failure is logged, never returned
// — a job's durable-execution outcome must not depend on object
// storage being reachable (the same reasoning as I-8 for Redis).
// Returns the exported archive's bytes on success, or nil — the same
// bytes deployPreview needs, so a job with options.deploy set doesn't
// pay for a second ExportWorkspace round-trip to the Runner.
func (e *Executor) uploadArtifact(ctx context.Context, job *queue.Job, sandboxID string) []byte {
	if e.artifacts == nil {
		return nil
	}
	tar, err := e.sandbox.ExportWorkspace(ctx, sandboxID)
	if err != nil {
		e.log.ErrorContext(ctx, "export workspace for artifact upload failed", slog.String("sandbox_id", sandboxID), slog.Any("err", err))
		return nil
	}
	defer func() { _ = tar.Close() }()

	archive, err := io.ReadAll(tar)
	if err != nil {
		e.log.ErrorContext(ctx, "read exported workspace failed", slog.String("sandbox_id", sandboxID), slog.Any("err", err))
		return nil
	}

	key, err := e.artifacts.Upload(ctx, job.ID, bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		e.log.ErrorContext(ctx, "upload artifact failed", slog.String("sandbox_id", sandboxID), slog.Any("err", err))
		return archive
	}
	if err := queue.SetJobArtifactKey(ctx, e.pool, job.ID, key); err != nil {
		e.log.ErrorContext(ctx, "persist artifact key failed", slog.Any("err", err))
	}
	return archive
}

// deployPreview builds job's already-uploaded archive into a preview
// deployment (PRD §13.1's DEPLOYING state), transitioning RUNNING ->
// DEPLOYING before starting and DEPLOYING -> SUCCEEDED or, on a deploy
// error, DEPLOYING -> FAILED (the "deploy error" edge in PRD §13.1's
// diagram) — a job that opted into a preview and didn't get one is a
// failed job, not a silently degraded success. Returns nil in both
// outcomes: the terminal transition has already been written, so
// RunStep must not also return an error dispatcher.go would try to
// transition again.
func (e *Executor) deployPreview(ctx context.Context, job *queue.Job, archive []byte) error {
	if archive == nil {
		return e.failDeploy(ctx, job, queue.StatusRunning, errors.New("no artifact was uploaded to deploy"))
	}
	if err := queue.Transition(ctx, e.pool, job.ID, queue.StatusRunning, queue.StatusDeploying, queue.JobStatusFields{}); err != nil {
		e.log.ErrorContext(ctx, "transition to DEPLOYING failed", slog.Any("err", err))
		return fmt.Errorf("agent: transition to deploying: %w", err)
	}
	e.publish(ctx, job.ID, "job_deploying", map[string]any{})

	previewURL, err := e.deployer.Deploy(ctx, job.ID, archive)
	if err != nil {
		return e.failDeploy(ctx, job, queue.StatusDeploying, err)
	}
	if err := queue.SetJobPreviewURL(ctx, e.pool, job.ID, previewURL); err != nil {
		e.log.ErrorContext(ctx, "persist preview url failed", slog.Any("err", err))
	}
	if err := queue.Transition(ctx, e.pool, job.ID, queue.StatusDeploying, queue.StatusSucceeded, queue.JobStatusFields{}); err != nil {
		e.log.ErrorContext(ctx, "transition to SUCCEEDED failed", slog.Any("err", err))
		return fmt.Errorf("agent: transition to succeeded: %w", err)
	}
	e.publish(ctx, job.ID, "job_succeeded", map[string]any{"preview_url": previewURL})
	return nil
}

// failDeploy transitions job from its current status (RUNNING if no
// archive was ever available, DEPLOYING if Deploy itself failed — the
// caller always knows exactly which, since it made the transition
// into DEPLOYING itself, if any) to FAILED with deployErr's message
// as the failure reason.
func (e *Executor) failDeploy(ctx context.Context, job *queue.Job, from queue.Status, deployErr error) error {
	e.log.ErrorContext(ctx, "deploy preview failed", slog.Any("err", deployErr))
	reason := fmt.Sprintf("deploy preview: %s", deployErr.Error())
	if err := queue.Transition(ctx, e.pool, job.ID, from, queue.StatusFailed, queue.JobStatusFields{FailureReason: &reason}); err != nil {
		e.log.ErrorContext(ctx, "transition to FAILED failed", slog.Any("err", err))
		return fmt.Errorf("agent: transition to failed: %w", err)
	}
	e.publish(ctx, job.ID, "job_failed", map[string]any{"reason": reason})
	return nil
}

// finishCancellation completes PRD §13.3 steps 3-4 for a cancellation
// this worker itself observed (as opposed to step 5's sweeper path for
// a wedged worker): the sandbox is disposable, so a forced remove is
// sufficient termination — there is nothing inside it worth a graceful
// shutdown — and the job lands in CANCELLED.
func (e *Executor) finishCancellation(ctx context.Context, job *queue.Job, sandboxID string) {
	if err := e.sandbox.Destroy(context.WithoutCancel(ctx), sandboxID); err != nil {
		e.log.ErrorContext(ctx, "destroy sandbox on cancel failed", slog.String("sandbox_id", sandboxID), slog.Any("err", err))
	}
	if err := queue.Transition(ctx, e.pool, job.ID, job.Status, queue.StatusCancelled, queue.JobStatusFields{}); err != nil {
		e.log.ErrorContext(ctx, "transition to CANCELLED failed", slog.Any("err", err))
	}
	e.publish(ctx, job.ID, "job_cancelled", map[string]any{})
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

func (e *Executor) runAllSteps(ctx context.Context, job *queue.Job, sandboxID string) error {
	steps, err := queue.ListSteps(ctx, e.pool, job.ID)
	if err != nil {
		return fmt.Errorf("agent: list steps for job %s: %w", job.ID, err)
	}

	for _, step := range steps {
		if step.Status == queue.StepSucceeded || step.Status == queue.StepSkipped {
			continue
		}
		cancelled, err := e.cancel.Cancelled(ctx, job.ID)
		if err != nil {
			return fmt.Errorf("agent: check cancellation: %w", err)
		}
		if cancelled {
			return ErrCancelled
		}
		if err := e.runOneStep(ctx, job, steps, step, sandboxID); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) runOneStep(ctx context.Context, job *queue.Job, steps []queue.Step, step queue.Step, sandboxID string) error {
	ctx, span := telemetry.Tracer("agent").Start(ctx, "step.execute", trace.WithAttributes(
		telemetry.AttrJobID.String(job.ID.String()),
		telemetry.AttrStepIdx.Int(step.Idx),
	))
	defer span.End()

	if err := e.runOneStepBody(ctx, job, steps, step, sandboxID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// runOneStepBody is runOneStep's actual logic, split out so the span
// wrapper above stays a thin, uniform shape.
func (e *Executor) runOneStepBody(ctx context.Context, job *queue.Job, steps []queue.Step, step queue.Step, sandboxID string) error {
	if err := queue.StartStep(ctx, e.pool, step.ID); err != nil {
		return fmt.Errorf("agent: start step %d: %w", step.Idx, err)
	}
	e.publish(ctx, job.ID, "step_started", map[string]any{"step_idx": step.Idx, "title": step.Title})

	success, summary, err := e.runTurnLoop(ctx, job, steps, step, sandboxID)
	if errors.Is(err, ErrCancelled) {
		// Leave the step RUNNING: this step didn't fail, the job is
		// being torn down entirely, and finishCancellation (or the
		// sweeper's wedged-worker path) owns what happens next.
		return err
	}
	if err != nil {
		return e.finishStepUnsuccessful(ctx, job.ID, step, err.Error())
	}
	if !success {
		return e.finishStepUnsuccessful(ctx, job.ID, step, summary)
	}

	if err := queue.FinishStep(ctx, e.pool, step.ID, queue.StepSucceeded, ""); err != nil {
		return fmt.Errorf("agent: finish step %d: %w", step.Idx, err)
	}
	e.publish(ctx, job.ID, "step_finished", map[string]any{"step_idx": step.Idx, "status": "SUCCEEDED"})
	return nil
}

// finishStepUnsuccessful lands step in FAILED or, if it was marked
// Optional by the planner, SKIPPED — an optional step's exhaustion
// does not fail the job (PRD §12.4).
func (e *Executor) finishStepUnsuccessful(ctx context.Context, jobID uuid.UUID, step queue.Step, reason string) error {
	status := queue.StepFailed
	if step.Optional {
		status = queue.StepSkipped
	}
	if err := queue.FinishStep(ctx, e.pool, step.ID, status, reason); err != nil {
		return fmt.Errorf("agent: finish step %d: %w", step.Idx, err)
	}
	e.publish(ctx, jobID, "step_finished", map[string]any{"step_idx": step.Idx, "status": string(status)})
	if step.Optional {
		return nil // the job continues past an optional step's failure
	}
	return fmt.Errorf("agent: step %d (%s): %s", step.Idx, step.Title, reason)
}

// runTurnLoop drives the executor turn loop for one step: call the
// model, persist-then-dispatch its one tool call, feed the observation
// back, repeat until step_done or MaxTurnsPerStep is reached.
//
// Cancellation is checked BETWEEN EVERY TURN, not only between steps
// (PRD §13.3 step 2) — a step with MaxTurnsPerStep turns can run for
// minutes, and a cancel that waits for the step boundary is not a
// cancel. A repair turn (following a failed exec call) counts against
// MaxTurnsPerStep like any other turn — repairs and turns share one
// clock, and the step ends at whichever cap is hit first (Week 7 open
// question 1).
func (e *Executor) runTurnLoop(ctx context.Context, job *queue.Job, steps []queue.Step, step queue.Step, sandboxID string) (success bool, summary string, err error) {
	retryHint := ""
	for turnIdx := 0; turnIdx < e.maxTurns; turnIdx++ {
		cancelled, err := e.cancel.Cancelled(ctx, job.ID)
		if err != nil {
			return false, "", fmt.Errorf("agent: check cancellation: %w", err)
		}
		if cancelled {
			return false, "", ErrCancelled
		}

		stepTurnsTotal.Inc()
		outcome, loopErr := e.runOneTurn(ctx, job, steps, step, sandboxID, turnIdx, retryHint)
		if loopErr != nil {
			return false, "", loopErr
		}
		if outcome.done {
			return outcome.success, outcome.summary, nil
		}
		retryHint = outcome.retryHint
	}
	return false, "", fmt.Errorf("%w: step %d after %d turns", ErrStepTurnLimitExceeded, step.Idx, e.maxTurns)
}

type turnOutcome struct {
	done      bool
	success   bool
	summary   string
	retryHint string
}
