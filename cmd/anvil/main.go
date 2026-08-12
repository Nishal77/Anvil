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

	"golang.org/x/sync/errgroup"

	"github.com/anvil-dev/anvil/internal/api"
	"github.com/anvil-dev/anvil/internal/auth"
	"github.com/anvil-dev/anvil/internal/config"
	"github.com/anvil-dev/anvil/internal/events"
	"github.com/anvil-dev/anvil/internal/executor"
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

	exec, err := executor.New(executor.Config{
		Sandbox:   sandboxClient,
		Publisher: publisher,
		Pool:      store.Pool(),
		Logger:    log,
	})
	if err != nil {
		return nil, fmt.Errorf("construct executor: %w", err)
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
