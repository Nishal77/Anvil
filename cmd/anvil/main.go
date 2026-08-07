// Command anvil is the control plane binary: HTTP + SSE edge, durable
// queue, and agent runtime in a single process. See CLAUDE.md §4.1.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/anvil-dev/anvil/internal/api"
	"github.com/anvil-dev/anvil/internal/auth"
	"github.com/anvil-dev/anvil/internal/config"
	"github.com/anvil-dev/anvil/internal/storage"
	"github.com/anvil-dev/anvil/internal/telemetry"
)

func main() {
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

	store, err := storage.New(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer store.Close()

	authSvc, err := auth.New(auth.Config{
		Store:           store,
		Logger:          log,
		JWTSigningKey:   cfg.JWTSigningKey,
		AccessTokenTTL:  cfg.AccessTokenTTL,
		RefreshTokenTTL: cfg.RefreshTokenTTL,
	})
	if err != nil {
		return fmt.Errorf("construct auth service: %w", err)
	}

	server, err := api.New(api.Config{
		Addr:   cfg.HTTPAddr,
		Auth:   authSvc,
		Store:  store,
		Logger: log,
	})
	if err != nil {
		return fmt.Errorf("construct api server: %w", err)
	}

	log.Info("starting", slog.String("addr", cfg.HTTPAddr))
	if err := server.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	return nil
}
