package bench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/sandbox"
)

// placeholderSystemPrompt is deliberately unrefined — Week 5 has no
// prompt tuning (BUILD-PLAN anti-goal). It exists so the harness can
// run end to end and produce a real, if low, baseline number.
const placeholderSystemPrompt = "You are a coding agent. Given a task description, call write_file one or more times to create every file the task needs, then stop. Write complete, compilable file contents — do not truncate or summarize them."

const (
	sandboxTimeout   = 5 * time.Minute
	execTimeout      = 5 * time.Minute
	writeFileTimeout = 30 * time.Second
)

var writeFileTool = llm.Tool{
	Name:        "write_file",
	Description: "Write a file's full contents into the workspace, creating parent directories as needed.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Path relative to the workspace root."},
			"content": {"type": "string", "description": "Complete file contents."}
		},
		"required": ["path", "content"]
	}`),
}

// sandboxRunner is the subset of *sandbox.Client the Runner needs,
// declared here (CODE-STANDARDS §3.1) so tests supply a fake instead
// of driving a real Docker-backed Runner for every case.
type sandboxRunner interface {
	Create(ctx context.Context, jobID uuid.UUID) (string, error)
	Exec(ctx context.Context, sandboxID, command string, timeout time.Duration, onChunk func(sandbox.ExecChunk)) error
	Destroy(ctx context.Context, sandboxID string) error
}

// Result is one task's outcome.
type Result struct {
	Task string
	Tier Tier
	// Passed is only meaningful when HarnessError is empty: a harness
	// fault (sandbox or LLM error) is not the same thing as the task
	// itself failing its check.
	Passed        bool
	HarnessError  string
	CostUSDMicros int64
	Usage         llm.Usage
	Duration      time.Duration
}

// Config wires a Runner's dependencies.
type Config struct {
	Router  *llm.Router
	Sandbox *sandbox.Client
	Logger  *slog.Logger
}

func (c Config) validate() error {
	if c.Router == nil {
		return errors.New("bench: config: Router is required")
	}
	if c.Sandbox == nil {
		return errors.New("bench: config: Sandbox is required")
	}
	if c.Logger == nil {
		return errors.New("bench: config: Logger is required")
	}
	return nil
}

// Runner drives each task end to end: one placeholder-prompt
// completion via Router, applying the model's write_file tool calls
// to a fresh sandbox, then running the task's Check inside it.
type Runner struct {
	router  *llm.Router
	sandbox sandboxRunner
	log     *slog.Logger
}

// NewRunner constructs a Runner from cfg, or returns an error if cfg
// is invalid.
func NewRunner(cfg Config) (*Runner, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Runner{router: cfg.Router, sandbox: cfg.Sandbox, log: cfg.Logger}, nil
}

// Run executes every task in order and returns one Result per task.
// One task erroring (sandbox failure, LLM error) becomes a failed
// Result with HarnessError set — it must not abort the other tasks,
// since one broken task hiding four working ones would make the
// benchmark number meaningless.
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

	jobID := uuid.New()
	createCtx, cancel := context.WithTimeout(ctx, sandboxTimeout)
	sandboxID, err := r.sandbox.Create(createCtx, jobID)
	cancel()
	if err != nil {
		result.HarnessError = fmt.Sprintf("create sandbox: %s", err)
		result.Duration = time.Since(start)
		return result
	}
	defer func() {
		if err := r.sandbox.Destroy(context.WithoutCancel(ctx), sandboxID); err != nil {
			r.log.Warn("destroy sandbox after benchmark task", "component", "bench", "task", task.Name, "error", err)
		}
	}()

	resp, err := r.router.Complete(ctx, jobID, llm.Request{
		TaskClass: llm.TaskPlanning,
		System:    placeholderSystemPrompt,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: task.Prompt}},
		Tools:     []llm.Tool{writeFileTool},
	})
	if err != nil {
		result.HarnessError = fmt.Sprintf("llm complete: %s", err)
		result.Duration = time.Since(start)
		return result
	}
	result.Usage = resp.Usage
	result.CostUSDMicros = llm.CostUSDMicros(resp.Model, resp.Usage)

	if err := r.applyToolCalls(ctx, sandboxID, resp.ToolCalls); err != nil {
		result.HarnessError = fmt.Sprintf("apply tool calls: %s", err)
		result.Duration = time.Since(start)
		return result
	}

	passed, err := r.runCheck(ctx, sandboxID, task.Check)
	if err != nil {
		result.HarnessError = fmt.Sprintf("run check: %s", err)
		result.Duration = time.Since(start)
		return result
	}
	result.Passed = passed
	result.Duration = time.Since(start)
	return result
}

// applyToolCalls writes every write_file call's content into the
// sandbox workspace. Content is base64-encoded client-side and
// decoded inside the sandbox rather than interpolated into a shell
// heredoc, so arbitrary file content (including a literal heredoc
// terminator) can never break out of the command.
func (r *Runner) applyToolCalls(ctx context.Context, sandboxID string, calls []llm.ToolCall) error {
	for _, call := range calls {
		if call.Name != writeFileTool.Name {
			continue // Week 5 only offers one tool; an unrecognized call is silently ignored, not fatal
		}
		var input struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(call.Input, &input); err != nil {
			return fmt.Errorf("decode write_file input: %w", err)
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(input.Content))
		command := fmt.Sprintf(
			"mkdir -p \"$(dirname %q)\" && echo %s | base64 -d > %q",
			input.Path, encoded, input.Path,
		)
		exitCode, err := r.exec(ctx, sandboxID, command, writeFileTimeout)
		if err != nil {
			return fmt.Errorf("write %s: %w", input.Path, err)
		}
		if exitCode != 0 {
			return fmt.Errorf("write %s: exit code %d", input.Path, exitCode)
		}
	}
	return nil
}

func (r *Runner) runCheck(ctx context.Context, sandboxID string, check []string) (bool, error) {
	if len(check) == 0 {
		return false, errors.New("task has an empty check command")
	}
	exitCode, err := r.exec(ctx, sandboxID, strings.Join(check, " "), execTimeout)
	if err != nil {
		return false, err // harness couldn't even run the check
	}
	return exitCode == 0, nil // the check ran; a nonzero exit is a task failure, not a harness fault
}

// exec runs command in sandboxID and reports its exit code. A non-nil
// error means the harness itself could not complete the command
// (timeout, sandbox gone) — distinct from the command running and
// exiting nonzero, which callers read from exitCode.
func (r *Runner) exec(ctx context.Context, sandboxID, command string, timeout time.Duration) (exitCode int, err error) {
	err = r.sandbox.Exec(ctx, sandboxID, command, timeout, func(chunk sandbox.ExecChunk) {
		if chunk.Final {
			exitCode = chunk.ExitCode
		}
	})
	if err != nil {
		return 0, fmt.Errorf("bench: exec: %w", err)
	}
	return exitCode, nil
}
