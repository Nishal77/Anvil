package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/agent"
	"github.com/anvil-dev/anvil/internal/artifact"
	"github.com/anvil-dev/anvil/internal/bench"
	"github.com/anvil-dev/anvil/internal/config"
	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/sandbox"
	"github.com/anvil-dev/anvil/internal/storage"
)

// benchUserEmail is the fixed account every benchmark job is created
// under — the benchmark harness doesn't need a real user, just a
// stable foreign key for jobs.user_id.
const benchUserEmail = "bench@anvil.internal"

// benchEnv is the sandbox's actual shape, same as cmd/anvil's
// plannerEnv — the benchmark harness runs against the identical image
// production does.
var benchEnv = agent.EnvDescription{
	Image:     "anvil-workspace",
	Languages: []string{"go1.23"},
	Network:   "allowlist: proxy.golang.org, github.com",
}

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

	runner, closeFn, err := newBenchRunner(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer closeFn()

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

// newBenchRunner builds a bench.Runner driving the real Planner and
// Executor — the same pipeline production uses, not a single-shot
// stand-in — against real Postgres and object storage. Returns a
// close func the caller must run once done (releases the pool).
// Split out of runBench purely to keep that function's cyclomatic
// complexity under CLAUDE.md §5.1's limit.
func newBenchRunner(ctx context.Context, cfg config.BenchConfig, log *slog.Logger) (*bench.Runner, func(), error) {
	store, err := storage.New(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to database: %w", err)
	}
	closeFn := func() { store.Close() }

	sandboxClient, err := sandbox.New(sandbox.Config{RunnerAddr: cfg.RunnerAddr, Logger: log})
	if err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("configure sandbox client: %w", err)
	}

	artifacts, err := artifact.New(ctx, artifact.Config{
		Endpoint: cfg.S3Endpoint, Bucket: cfg.S3Bucket,
		AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey, UseSSL: cfg.S3UseSSL,
	})
	if err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("configure artifact store: %w", err)
	}

	router, err := newBenchRouter(ctx, cfg, store, log)
	if err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("configure llm router: %w", err)
	}

	exec, planner, err := newBenchAgent(cfg, sandboxClient, store, artifacts, router, log)
	if err != nil {
		closeFn()
		return nil, nil, err
	}

	userID, err := ensureBenchUser(ctx, store)
	if err != nil {
		closeFn()
		return nil, nil, err
	}

	runner, err := bench.NewRunner(bench.Config{
		Pool: store.Pool(), Turns: store, Artifacts: artifacts,
		Planner: planner, Executor: exec, UserID: userID, Env: benchEnv, Logger: log,
	})
	if err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("configure benchmark runner: %w", err)
	}
	return runner, closeFn, nil
}

// newBenchAgent builds the tool registry, policy engine, Executor,
// and Planner — everything internal/agent needs, wired the same way
// cmd/anvil's wireAgent does.
func newBenchAgent(cfg config.BenchConfig, sandboxClient *sandbox.Client, store *storage.Store, artifacts *artifact.Store, router *llm.Router, log *slog.Logger) (*agent.Executor, *agent.Planner, error) {
	registry, err := agent.NewRegistry(append(
		agent.NewFSTools(sandboxClient),
		agent.NewExecTool(sandboxClient),
		agent.NewStepDoneTool(),
	)...)
	if err != nil {
		return nil, nil, fmt.Errorf("construct tool registry: %w", err)
	}

	budget := llm.NewInMemoryBudgetStore(defaultJobTokenBudget)
	policy, err := agent.NewPolicyEngine(agent.PolicyEngineConfig{Registry: registry, Sandbox: sandboxClient, Budget: budget})
	if err != nil {
		return nil, nil, fmt.Errorf("construct policy engine: %w", err)
	}

	exec, err := agent.New(agent.Config{
		Registry: registry, Policy: policy, Router: router, Sandbox: sandboxClient,
		Publisher: noopPublisher{}, IdemStore: store, Turns: store,
		Pool: store.Pool(), Logger: log, Artifacts: artifacts,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("construct executor: %w", err)
	}

	planner, err := agent.NewPlanner(agent.PlannerConfig{Router: router, Pool: store.Pool(), MaxSteps: cfg.MaxSteps, Logger: log})
	if err != nil {
		return nil, nil, fmt.Errorf("construct planner: %w", err)
	}
	return exec, planner, nil
}

const defaultJobTokenBudget = 150_000

// noopPublisher discards every event — the benchmark harness has no
// SSE subscriber to stream to, and losing that stream is not a reason
// to fail a benchmark run.
type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, uuid.UUID, storage.EventType, json.RawMessage) error {
	return nil
}

// ensureBenchUser returns benchUserEmail's user ID, creating the row
// on first run. A fixed password hash (this account is never logged
// into) keeps repeated bench runs idempotent without a real password.
func ensureBenchUser(ctx context.Context, store *storage.Store) (uuid.UUID, error) {
	existing, err := store.GetUserByEmail(ctx, benchUserEmail)
	if err == nil {
		return existing.ID, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return uuid.Nil, fmt.Errorf("look up bench user: %w", err)
	}

	sum := sha256.Sum256([]byte(benchUserEmail))
	created, err := store.CreateUser(ctx, benchUserEmail, "bench:"+hex.EncodeToString(sum[:]))
	if err != nil {
		return uuid.Nil, fmt.Errorf("create bench user: %w", err)
	}
	return created.ID, nil
}

// newBenchRouter builds a Router with Anthropic primary / OpenAI
// fallback for TaskPlanning, TaskExecution, and TaskSummarization —
// the same three task classes cmd/anvil's router serves, since the
// benchmark harness now drives the real Planner+Executor pipeline
// rather than one completion call. Gemini is appended last, only if
// configured; it is not required.
func newBenchRouter(ctx context.Context, cfg config.BenchConfig, spend llm.SpendReader, log *slog.Logger) (*llm.Router, error) {
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
		Providers: map[llm.TaskClass][]llm.Provider{
			llm.TaskPlanning:      ladder,
			llm.TaskExecution:     ladder,
			llm.TaskSummarization: ladder,
		},
		Budget: llm.NewInMemoryBudgetStore(defaultJobTokenBudget),
		Cap:    llm.NewGlobalCap(spend, 0, log),
		Logger: log,
	})
	if err != nil {
		return nil, fmt.Errorf("new router: %w", err)
	}
	return router, nil
}
