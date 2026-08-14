// Command anvil is the control plane binary: HTTP + SSE edge, durable
// queue, and agent runtime in a single process. See CLAUDE.md §4.1.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/anvil-dev/anvil/internal/agent"
	"github.com/anvil-dev/anvil/internal/api"
	"github.com/anvil-dev/anvil/internal/auth"
	"github.com/anvil-dev/anvil/internal/config"
	"github.com/anvil-dev/anvil/internal/events"
	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/sandbox"
	"github.com/anvil-dev/anvil/internal/storage"
	"github.com/anvil-dev/anvil/internal/telemetry"
	"github.com/anvil-dev/anvil/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		// A failed write to stdout on a --version flag leaves nothing
		// useful to do; there is no error path to report it through.
		_, _ = fmt.Fprintln(os.Stdout, version.String())
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log := telemetry.NewLogger(os.Stdout, "api")

	cp, err := wireControlPlane(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer cp.close()

	// Server, Dispatcher, and Hub each own their own goroutines and all
	// stop cleanly on ctx cancellation; if any one of them returns an
	// error, the others are cancelled too rather than left running
	// half the control plane.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return cp.hub.Run(gctx, cp.redis) })
	g.Go(func() error { return cp.dispatcher.Run(gctx) })
	g.Go(func() error {
		log.Info("starting", slog.String("addr", cfg.HTTPAddr))
		return cp.server.Run(gctx)
	})

	if err := g.Wait(); err != nil {
		return fmt.Errorf("run control plane: %w", err)
	}
	return nil
}

// controlPlane holds every long-lived component run wires together and
// then hands to the errgroup — kept as one struct so run() itself stays
// under the complexity limit CI enforces on every function.
type controlPlane struct {
	store      *storage.Store
	redis      *events.RedisClient
	dispatcher *queue.Dispatcher
	hub        *events.Hub
	server     *api.Server
}

func (cp *controlPlane) close() {
	_ = cp.redis.Close()
	cp.store.Close()
}

func wireControlPlane(ctx context.Context, cfg config.Config, log *slog.Logger) (*controlPlane, error) {
	store, err := storage.New(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	redisClient := events.NewRedisClient(cfg.RedisAddr)

	authSvc, err := auth.New(auth.Config{
		Store:           store,
		Logger:          log,
		JWTSigningKey:   cfg.JWTSigningKey,
		AccessTokenTTL:  cfg.AccessTokenTTL,
		RefreshTokenTTL: cfg.RefreshTokenTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("construct auth service: %w", err)
	}

	publisher, err := events.New(events.Config{Store: store, Redis: redisClient, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("construct event publisher: %w", err)
	}

	hub, err := events.NewHub(events.HubConfig{Logger: log})
	if err != nil {
		return nil, fmt.Errorf("construct event hub: %w", err)
	}

	sandboxClient, err := sandbox.New(sandbox.Config{RunnerAddr: cfg.RunnerAddr, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("construct sandbox client: %w", err)
	}

	exec, err := wireExecutor(ctx, cfg, sandboxClient, publisher, store, log)
	if err != nil {
		return nil, err
	}

	dispatcher, err := queue.New(queue.Config{
		Pool:    store.Pool(),
		Logger:  log,
		RunStep: exec.RunStep,
	})
	if err != nil {
		return nil, fmt.Errorf("construct dispatcher: %w", err)
	}

	server, err := api.New(api.Config{
		Addr:       cfg.HTTPAddr,
		Auth:       authSvc,
		Store:      store,
		Pool:       store.Pool(),
		Hub:        hub,
		EventStore: store,
		Publisher:  publisher,
		Logger:     log,
	})
	if err != nil {
		return nil, fmt.Errorf("construct api server: %w", err)
	}

	return &controlPlane{store: store, redis: redisClient, dispatcher: dispatcher, hub: hub, server: server}, nil
}

// wireExecutor builds the agent.Executor: the tool registry (§12.2's
// SAFE/GUARDED tools — no git tools, no PRIVILEGED tools until Week 8),
// the policy engine, an llm.Router over the configured provider ladder,
// and the storage-backed idempotency/agent_turns stores.
func wireExecutor(ctx context.Context, cfg config.Config, sandboxClient *sandbox.Client, pub *events.Publisher, store *storage.Store, log *slog.Logger) (*agent.Executor, error) {
	registry, err := agent.NewRegistry(append(
		agent.NewFSTools(sandboxClient),
		agent.NewExecTool(sandboxClient),
		agent.NewStepDoneTool(),
	)...)
	if err != nil {
		return nil, fmt.Errorf("construct tool registry: %w", err)
	}

	budget := storageBudget{store: store}
	router, err := wireRouter(ctx, cfg, budget, store, log)
	if err != nil {
		return nil, err
	}

	policy, err := agent.NewPolicyEngine(agent.PolicyEngineConfig{
		Registry: registry,
		Sandbox:  sandboxClient,
		Budget:   budget,
	})
	if err != nil {
		return nil, fmt.Errorf("construct policy engine: %w", err)
	}

	exec, err := agent.New(agent.Config{
		Registry:  registry,
		Policy:    policy,
		Router:    router,
		Sandbox:   sandboxClient,
		Publisher: pub,
		IdemStore: store,
		Turns:     store,
		Pool:      store.Pool(),
		Logger:    log,
	})
	if err != nil {
		return nil, fmt.Errorf("construct executor: %w", err)
	}
	return exec, nil
}

// wireRouter builds an llm.Router with Anthropic primary / OpenAI
// fallback for TaskExecution — the only task class the Week 6 executor
// loop issues (TaskPlanning is Week 7's). Gemini is appended last,
// only if configured — it is not required (ANVIL_GEMINI_API_KEY may
// be left unset). Budget is the same storageBudget instance the
// policy engine's rule 6 reads, so both are checking one source of
// truth, not two budgets that can disagree.
func wireRouter(ctx context.Context, cfg config.Config, budget llm.BudgetStore, spend llm.SpendReader, log *slog.Logger) (*llm.Router, error) {
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
		Providers: map[llm.TaskClass][]llm.Provider{llm.TaskExecution: ladder},
		Budget:    budget,
		Cap:       llm.NewGlobalCap(spend, cfg.MonthlyUSDCapMicros, log),
		Logger:    log,
	})
	if err != nil {
		return nil, fmt.Errorf("construct llm router: %w", err)
	}
	return router, nil
}

// storageBudget adapts *storage.Store's primitive-typed job-budget
// methods to llm.BudgetStore. Lives here, not in internal/storage,
// because storage may not import llm (CLAUDE.md PK5); lives here
// rather than internal/agent because it is a one-call piece of wiring,
// not a reusable abstraction.
type storageBudget struct {
	store *storage.Store
}

func (b storageBudget) GetJobBudget(ctx context.Context, jobID uuid.UUID) (llm.JobBudget, error) {
	tokenBudget, tokensUsed, err := b.store.GetJobTokenBudget(ctx, jobID)
	if err != nil {
		return llm.JobBudget{}, fmt.Errorf("storage budget: get job budget: %w", err)
	}
	return llm.JobBudget{TokenBudget: tokenBudget, TokensUsed: tokensUsed}, nil
}

func (b storageBudget) AddJobUsage(ctx context.Context, jobID uuid.UUID, tokens, costUSDMicros int64) error {
	if err := b.store.AddJobTokenUsage(ctx, jobID, tokens, costUSDMicros); err != nil {
		return fmt.Errorf("storage budget: add job usage: %w", err)
	}
	return nil
}
