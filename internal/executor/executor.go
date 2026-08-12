package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/sandbox"
	"github.com/anvil-dev/anvil/internal/storage"
)

const defaultExecTimeout = 60 * time.Second

// sandboxClient is the subset of sandbox.Client the executor needs.
// Declared here, at the consumer, per CODE-STANDARDS §3.1.
type sandboxClient interface {
	Create(ctx context.Context, jobID uuid.UUID) (string, error)
	Exec(ctx context.Context, sandboxID, command string, timeout time.Duration, onChunk func(sandbox.ExecChunk)) error
	Destroy(ctx context.Context, sandboxID string) error
}

// publisher is the subset of events.Publisher the executor needs.
type publisher interface {
	Publish(ctx context.Context, jobID uuid.UUID, typ storage.EventType, payload json.RawMessage) error
}

// Config configures an Executor.
type Config struct {
	Sandbox     sandboxClient
	Publisher   publisher
	Pool        *pgxpool.Pool // steps and jobs.sandbox_id live in queue's tables, read/written directly
	Logger      *slog.Logger
	ExecTimeout time.Duration // per-command timeout; default 60s
}

func (c *Config) setDefaults() {
	if c.ExecTimeout <= 0 {
		c.ExecTimeout = defaultExecTimeout
	}
}

func (c Config) validate() error {
	if c.Sandbox == nil {
		return errors.New("executor: config: Sandbox is required")
	}
	if c.Publisher == nil {
		return errors.New("executor: config: Publisher is required")
	}
	if c.Pool == nil {
		return errors.New("executor: config: Pool is required")
	}
	if c.Logger == nil {
		return errors.New("executor: config: Logger is required")
	}
	return nil
}

// Executor runs the fixed hardcodedPlan for a job inside one sandbox,
// step by step, publishing an event for every step transition and every
// line of command output.
type Executor struct {
	sandbox     sandboxClient
	pub         publisher
	pool        *pgxpool.Pool
	log         *slog.Logger
	execTimeout time.Duration
}

// New constructs an Executor from cfg, or returns an error if cfg is
// invalid.
func New(cfg Config) (*Executor, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Executor{
		sandbox:     cfg.Sandbox,
		pub:         cfg.Publisher,
		pool:        cfg.Pool,
		log:         cfg.Logger,
		execTimeout: cfg.ExecTimeout,
	}, nil
}

// hardcodedStep is one step of Week 4's fixed plan: no LLM, no tool
// abstraction, just a shell command to run in order.
type hardcodedStep struct {
	Title       string
	Description string
	Command     string
}

var hardcodedPlan = []hardcodedStep{
	{
		Title:       "Create app directory",
		Description: "Create the workspace app directory",
		Command:     "mkdir -p /workspace/app",
	},
	{
		Title:       "Write placeholder main.go",
		Description: "Write a minimal Go program into the app directory",
		Command:     `sh -c 'printf "package main\n\nfunc main() {}\n" > /workspace/app/main.go'`,
	},
	{
		Title:       "List app directory",
		Description: "Confirm the expected files exist",
		Command:     "ls -la /workspace/app",
	},
}

// RunStep matches queue.Config.RunStep's signature exactly, so the
// Dispatcher can call it directly with no adapter. It runs every step of
// hardcodedPlan against one sandbox, in order, skipping any step already
// SUCCEEDED — that's what makes resuming after a crash safe: a step's
// status in Postgres is the only thing RunStep trusts about what already
// happened.
func (e *Executor) RunStep(ctx context.Context, job *queue.Job) error {
	sandboxID, err := e.ensureSandbox(ctx, job)
	if err != nil {
		return err
	}

	runErr := e.runAllSteps(ctx, job.ID, sandboxID)

	// Torn down whenever we're not leaving this job for someone else to
	// resume — that's true whether every step succeeded or one of them
	// genuinely failed, not just on success. The one case that must NOT
	// destroy it is ctx being cancelled mid-step (the process shutting
	// down): the job gets reclaimed and resumed by another worker against
	// this same sandbox, and destroying it now would throw away
	// everything the steps so far already did.
	if ctx.Err() == nil {
		if err := e.sandbox.Destroy(context.Background(), sandboxID); err != nil { //nolint:contextcheck // reason: a fresh context on purpose — ctx may be near its deadline, but a job that finished cleanly still needs its sandbox torn down
			e.log.ErrorContext(ctx, "destroy sandbox failed", slog.String("sandbox_id", sandboxID), slog.Any("err", err))
		}
	}
	return runErr
}

func (e *Executor) runAllSteps(ctx context.Context, jobID uuid.UUID, sandboxID string) error {
	for idx, hs := range hardcodedPlan {
		step, err := queue.EnsureStep(ctx, e.pool, jobID, idx, hs.Title, hs.Description)
		if err != nil {
			return fmt.Errorf("executor: ensure step %d: %w", idx, err)
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

// ensureSandbox returns job's existing sandbox if it already has one
// (set by an earlier, possibly crashed, attempt at this same job), or
// creates a fresh one and persists its ID.
func (e *Executor) ensureSandbox(ctx context.Context, job *queue.Job) (string, error) {
	if job.SandboxID != "" {
		return job.SandboxID, nil
	}

	sandboxID, err := e.sandbox.Create(ctx, job.ID)
	if err != nil {
		return "", fmt.Errorf("executor: create sandbox: %w", err)
	}
	if err := queue.SetJobSandboxID(ctx, e.pool, job.ID, sandboxID); err != nil {
		return "", fmt.Errorf("executor: persist sandbox id: %w", err)
	}
	return sandboxID, nil
}

// runOneStep runs a single step's command, streaming its output as
// log_line events, and marks the step SUCCEEDED or FAILED in Postgres
// based on the real exit code.
func (e *Executor) runOneStep(ctx context.Context, jobID uuid.UUID, sandboxID string, step queue.Step, hs hardcodedStep) error {
	if err := queue.StartStep(ctx, e.pool, step.ID); err != nil {
		return fmt.Errorf("executor: start step %d: %w", step.Idx, err)
	}
	e.publish(ctx, jobID, "step_started", map[string]any{"step_idx": step.Idx, "title": hs.Title})

	var exitCode int
	onChunk := func(c sandbox.ExecChunk) {
		if c.Final {
			exitCode = c.ExitCode
			return
		}
		e.publish(ctx, jobID, "log_line", map[string]any{
			"stream": c.Stream, "text": string(c.Data), "step_idx": step.Idx,
		})
	}

	execErr := e.sandbox.Exec(ctx, sandboxID, hs.Command, e.execTimeout, onChunk)
	if execErr == nil && exitCode != 0 {
		execErr = fmt.Errorf("command exited %d", exitCode)
	}

	if execErr != nil {
		if err := queue.FinishStep(ctx, e.pool, step.ID, queue.StepFailed, execErr.Error()); err != nil {
			return fmt.Errorf("executor: finish step %d: %w", step.Idx, err)
		}
		e.publish(ctx, jobID, "step_finished", map[string]any{"step_idx": step.Idx, "status": "FAILED"})
		return fmt.Errorf("executor: step %d (%s): %w", step.Idx, hs.Title, execErr)
	}

	if err := queue.FinishStep(ctx, e.pool, step.ID, queue.StepSucceeded, ""); err != nil {
		return fmt.Errorf("executor: finish step %d: %w", step.Idx, err)
	}
	e.publish(ctx, jobID, "step_finished", map[string]any{"step_idx": step.Idx, "status": "SUCCEEDED"})
	return nil
}

// publish encodes data and sends it as one event of type typ for jobID.
// A failure here is logged, not returned — losing a log line or status
// marker from the live stream is not a reason to fail the job; the job's
// real state lives in the jobs and steps tables, not in job_events.
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
