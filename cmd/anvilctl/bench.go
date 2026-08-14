package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/anvil-dev/anvil/internal/bench"
	"github.com/anvil-dev/anvil/internal/config"
	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/sandbox"
)

const defaultJobTokenBudget = 150_000

// runBench wires internal/bench's Runner from flags and environment,
// runs every task under --tasks, and appends the results to --out
// (PRD §20.5). No LLM/sandbox/benchmark logic lives here — this
// function is wiring only (CLAUDE.md F8), same as main.go.
func runBench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	tasksDir := fs.String("tasks", "benchmarks/tasks", "directory of benchmark task subdirectories")
	outPath := fs.String("out", "benchmarks/results.md", "results file to append the run to")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	cfg, err := config.LoadBench()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	runner, err := newBenchRunner(ctx, cfg, log)
	if err != nil {
		return err
	}

	tasks, err := bench.LoadTasks(*tasksDir)
	if err != nil {
		return fmt.Errorf("load tasks: %w", err)
	}
	log.Info("running benchmark suite", "component", "anvilctl", "tasks", len(tasks))

	results, err := runner.Run(ctx, tasks)
	if err != nil {
		return fmt.Errorf("run benchmark suite: %w", err)
	}

	out, err := os.OpenFile(*outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", *outPath, err)
	}
	defer func() {
		if closeErr := out.Close(); closeErr != nil {
			log.Warn("close results file", "component", "anvilctl", "error", closeErr)
		}
	}()

	if err := bench.WriteResultsMarkdown(out, results, time.Now()); err != nil {
		return fmt.Errorf("write results: %w", err)
	}
	return nil
}

// newBenchRunner builds the Router and sandbox.Client a bench.Runner
// needs from cfg. Split out of runBench purely to keep that function's
// cyclomatic complexity under CLAUDE.md §5.1's limit — no behavior
// difference from inlining it.
func newBenchRunner(ctx context.Context, cfg config.BenchConfig, log *slog.Logger) (*bench.Runner, error) {
	router, err := newBenchRouter(ctx, cfg, log)
	if err != nil {
		return nil, fmt.Errorf("configure llm router: %w", err)
	}

	sandboxClient, err := sandbox.New(sandbox.Config{RunnerAddr: cfg.RunnerAddr, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("configure sandbox client: %w", err)
	}

	runner, err := bench.NewRunner(bench.Config{Router: router, Sandbox: sandboxClient, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("configure benchmark runner: %w", err)
	}
	return runner, nil
}

// newBenchRouter builds a Router with Anthropic primary / OpenAI
// fallback for the PLANNING task class — the only class the
// benchmark harness uses, per its single-shot design. Gemini is
// appended last, only if configured; it is not required.
func newBenchRouter(ctx context.Context, cfg config.BenchConfig, log *slog.Logger) (*llm.Router, error) {
	var ladder []llm.Provider

	if cfg.AnthropicAPIKey != "" {
		ladder = append(ladder, llm.NewAnthropicProvider(cfg.AnthropicAPIKey, anthropic.ModelClaudeHaiku4_5))
	}
	if cfg.OpenAIAPIKey != "" {
		ladder = append(ladder, llm.NewOpenAIProvider(cfg.OpenAIAPIKey, cfg.OpenAIModel))
	}
	if cfg.GeminiAPIKey != "" {
		provider, err := llm.NewGeminiProvider(ctx, cfg.GeminiAPIKey, cfg.GeminiModel)
		if err != nil {
			return nil, fmt.Errorf("configure gemini provider: %w", err)
		}
		ladder = append(ladder, provider)
	}

	router, err := llm.NewRouter(llm.Config{
		Providers: map[llm.TaskClass][]llm.Provider{llm.TaskPlanning: ladder},
		Budget:    llm.NewInMemoryBudgetStore(defaultJobTokenBudget),
		Logger:    log,
	})
	if err != nil {
		return nil, fmt.Errorf("new router: %w", err)
	}
	return router, nil
}
