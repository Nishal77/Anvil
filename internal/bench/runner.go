package bench

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anvil-dev/anvil/internal/agent"
	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
)

const (
	planClaimTimeout = time.Minute
	runClaimTimeout  = 10 * time.Minute
	checkTimeout     = 2 * time.Minute
)

// Result is one task's outcome.
type Result struct {
	Task string
	Tier Tier
	// Passed is only meaningful when HarnessError is empty: a harness
	// fault (planning/execution infrastructure error) is not the same
	// thing as the task itself failing its check.
	Passed        bool
	HarnessError  string
	CostUSDMicros int64
	Usage         llm.Usage
	Duration      time.Duration
}

// turnLister is the subset of *storage.Store the Runner needs to sum
// cost and token usage across every turn a job's real run produced
// (planner + every executor turn) — declared at the consumer per
// CODE-STANDARDS §3.1.
type turnLister interface {
	ListAgentTurns(ctx context.Context, jobID uuid.UUID) ([]storage.AgentTurn, error)
}

// artifactDownloader is the subset of *artifact.Store the Runner
// needs to independently verify a SUCCEEDED job's output, rather than
// trusting the agent's own self-report.
type artifactDownloader interface {
	Download(ctx context.Context, jobID uuid.UUID) (io.ReadCloser, error)
}

// Config wires a Runner's dependencies.
type Config struct {
	Pool      *pgxpool.Pool
	Turns     turnLister
	Artifacts artifactDownloader
	Planner   *agent.Planner
	Executor  *agent.Executor
	UserID    uuid.UUID // a seeded user every benchmark job is created under
	Env       agent.EnvDescription
	Logger    *slog.Logger
}

func (c Config) validate() error {
	for name, v := range map[string]bool{
		"Pool": c.Pool == nil, "Turns": c.Turns == nil, "Artifacts": c.Artifacts == nil,
		"Planner": c.Planner == nil, "Executor": c.Executor == nil, "Logger": c.Logger == nil,
	} {
		if v {
			return fmt.Errorf("bench: config: %s is required", name)
		}
	}
	if c.UserID == uuid.Nil {
		return errors.New("bench: config: UserID is required")
	}
	return nil
}

// Runner drives each task through the real pipeline this project
// builds — Planner then Executor, exactly as production does — and
// verifies the result by downloading the job's uploaded artifact and
// running the task's own check command against it independently,
// rather than trusting the agent's self-reported success.
type Runner struct {
	pool      *pgxpool.Pool
	turns     turnLister
	artifacts artifactDownloader
	planner   *agent.Planner
	executor  *agent.Executor
	userID    uuid.UUID
	env       agent.EnvDescription
	log       *slog.Logger
}

// NewRunner constructs a Runner from cfg, or returns an error if cfg
// is invalid.
func NewRunner(cfg Config) (*Runner, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Runner{
		pool: cfg.Pool, turns: cfg.Turns, artifacts: cfg.Artifacts,
		planner: cfg.Planner, executor: cfg.Executor, userID: cfg.UserID, env: cfg.Env, log: cfg.Logger,
	}, nil
}

// Run executes every task in order and returns one Result per task.
// One task erroring (planning failure, execution infrastructure fault)
// becomes a failed Result with HarnessError set — it must not abort
// the other tasks, since one broken task hiding four working ones
// would make the benchmark number meaningless.
func (r *Runner) Run(ctx context.Context, tasks []Task) ([]Result, error) {
	results := make([]Result, 0, len(tasks))
	for _, task := range tasks {
		results = append(results, r.runOne(ctx, task))
	}
	return results, nil
}

func (r *Runner) runOne(ctx context.Context, task Task) Result {
	start := time.Now()
	result := Result{Task: task.Name, Tier: task.Tier}

	jobID, err := r.runJobToTerminal(ctx, task)
	if err != nil {
		result.HarnessError = err.Error()
		result.Duration = time.Since(start)
		return result
	}

	r.sumUsage(ctx, jobID, &result)

	job, err := queue.GetJob(ctx, r.pool, jobID)
	if err != nil {
		result.HarnessError = fmt.Sprintf("reload job: %s", err)
		result.Duration = time.Since(start)
		return result
	}

	if job.Status == queue.StatusSucceeded {
		passed, err := r.runCheck(ctx, jobID, task.Check)
		if err != nil {
			result.HarnessError = fmt.Sprintf("run check: %s", err)
		} else {
			result.Passed = passed
		}
	}
	// Any other terminal status (FAILED, CANCELLED) is a genuine task
	// failure, not a harness fault: result.Passed stays false with no
	// HarnessError, exactly like a check that ran and returned nonzero.

	result.Duration = time.Since(start)
	return result
}

// runJobToTerminal drives one job through planning and execution —
// the same two claim-and-run phases queue.Dispatcher's worker loop
// performs, just single-threaded and synchronous here since the
// benchmark harness runs one task at a time. Returns once the job has
// reached a terminal status.
func (r *Runner) runJobToTerminal(ctx context.Context, task Task) (uuid.UUID, error) {
	job, err := queue.CreateJob(ctx, r.pool, r.userID, task.Prompt, true)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create job: %w", err)
	}

	planning, err := queue.Claim(ctx, r.pool, "bench", planClaimTimeout)
	if err != nil {
		return job.ID, fmt.Errorf("claim for planning: %w", err)
	}
	if planning.ID != job.ID {
		return job.ID, fmt.Errorf("claimed job %s, want %s — another worker is running concurrently against this database", planning.ID, job.ID)
	}
	if err := r.planner.RunPlan(ctx, planning, r.env); err != nil {
		return job.ID, fmt.Errorf("plan: %w", err)
	}

	running, err := queue.Claim(ctx, r.pool, "bench", runClaimTimeout)
	if err != nil {
		return job.ID, fmt.Errorf("claim for execution: %w", err)
	}
	if running.ID != job.ID {
		return job.ID, fmt.Errorf("claimed job %s, want %s — another worker is running concurrently against this database", running.ID, job.ID)
	}

	runErr := r.executor.RunStep(ctx, running)
	r.finishJob(ctx, job.ID, runErr)
	return job.ID, nil
}

// finishJob replicates queue.Dispatcher.runJob's post-RunStep
// transition: RunStep itself only ever moves a job to CANCELLED
// (finishCancellation) — SUCCEEDED and FAILED are the caller's
// responsibility, same as production's dispatcher.
func (r *Runner) finishJob(ctx context.Context, jobID uuid.UUID, runErr error) {
	current, err := queue.GetJob(ctx, r.pool, jobID)
	if err != nil {
		r.log.ErrorContext(ctx, "reload job after run failed", "component", "bench", "job_id", jobID, "err", err)
		return
	}
	if current.Status != queue.StatusRunning {
		return // already moved itself (e.g. CANCELLED) — nothing to do
	}

	if runErr != nil {
		reason := runErr.Error()
		if err := queue.Transition(ctx, r.pool, jobID, queue.StatusRunning, queue.StatusFailed, queue.JobStatusFields{FailureReason: &reason}); err != nil {
			r.log.ErrorContext(ctx, "transition to FAILED failed", "component", "bench", "job_id", jobID, "err", err)
		}
		return
	}
	if err := queue.Transition(ctx, r.pool, jobID, queue.StatusRunning, queue.StatusSucceeded, queue.JobStatusFields{}); err != nil {
		r.log.ErrorContext(ctx, "transition to SUCCEEDED failed", "component", "bench", "job_id", jobID, "err", err)
	}
}

// sumUsage totals cost and token usage across every agent_turns row
// the job produced — planning is one turn, execution is however many
// the turn loop and any repairs took, and the benchmark number should
// reflect the whole job, not one call.
func (r *Runner) sumUsage(ctx context.Context, jobID uuid.UUID, result *Result) {
	turns, err := r.turns.ListAgentTurns(ctx, jobID)
	if err != nil {
		r.log.ErrorContext(ctx, "list agent turns for cost summary failed", "component", "bench", "job_id", jobID, "err", err)
		return
	}
	for _, t := range turns {
		result.CostUSDMicros += t.CostUSDMicros
		result.Usage.InputTokens += int64(t.TokensIn)
		result.Usage.OutputTokens += int64(t.TokensOut)
	}
}

// runCheck downloads jobID's uploaded artifact, extracts it, and runs
// check against the extracted directory — independent verification,
// not the agent's own self-reported success.
func (r *Runner) runCheck(ctx context.Context, jobID uuid.UUID, check []string) (bool, error) {
	if len(check) == 0 {
		return false, errors.New("task has an empty check command")
	}

	archive, err := r.artifacts.Download(ctx, jobID)
	if err != nil {
		return false, fmt.Errorf("download artifact: %w", err)
	}
	defer func() { _ = archive.Close() }()

	dir, err := os.MkdirTemp("", "anvil-bench-"+jobID.String())
	if err != nil {
		return false, fmt.Errorf("create extraction dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if err := extractTarGz(archive, dir); err != nil {
		return false, fmt.Errorf("extract artifact: %w", err)
	}

	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, check[0], check[1:]...) //nolint:gosec // reason: check commands come from this repo's own benchmarks/tasks/*/check.json, not user input
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil // the check ran and failed — a task failure, not a harness fault
		}
		return false, fmt.Errorf("run check: %w", err)
	}
	return true, nil
}

// extractTarGz writes every regular file in a gzipped tar archive
// into dir, recreating directories as needed. Anything that isn't a
// regular file or directory (symlinks, devices) is rejected — a
// benchmark task's own output has no legitimate reason to contain one,
// and extracting a malicious symlink from model-influenced content is
// exactly the kind of path escape CLAUDE.md S5 exists to prevent.
func extractTarGz(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		if err := extractEntry(tr, header, dir); err != nil {
			return err
		}
	}
}

// extractEntry handles one tar entry. Split out of extractTarGz purely
// to keep that function's branching under CLAUDE.md's cognitive-
// complexity limit — no behavior difference from inlining it.
func extractEntry(tr *tar.Reader, header *tar.Header, dir string) error {
	target := filepath.Join(dir, filepath.Clean(string(filepath.Separator)+header.Name))
	if !strings.HasPrefix(target, dir+string(filepath.Separator)) && target != dir {
		return fmt.Errorf("tar entry %q escapes extraction directory", header.Name)
	}

	switch header.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // reason: extracted workspace content, not a sensitive path
			return fmt.Errorf("create dir %s: %w", target, err)
		}
	case tar.TypeReg:
		if err := extractFile(tr, target, header.Mode); err != nil {
			return err
		}
	default:
		return fmt.Errorf("tar entry %q: unsupported type %v", header.Name, header.Typeflag)
	}
	return nil
}

func extractFile(r io.Reader, target string, mode int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // reason: extracted workspace content, not a sensitive path
		return fmt.Errorf("create parent dir for %s: %w", target, err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode)) //nolint:gosec // reason: extracted workspace content into a caller-controlled temp dir, not a sensitive path
	if err != nil {
		return fmt.Errorf("create file %s: %w", target, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, r); err != nil { //nolint:gosec // reason: archive size is bounded by the sandbox's own disk quota (512m tmpfs), not attacker-controlled beyond that
		return fmt.Errorf("write file %s: %w", target, err)
	}
	return nil
}
