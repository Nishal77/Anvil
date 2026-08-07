// Command runner is the sandbox Runner: the only process that talks to
// Docker directly. See internal/sandbox/runner.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/anvil-dev/anvil/internal/sandbox/runner"
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

	log := telemetry.NewLogger(os.Stdout, "runner")

	addr := envOr("ANVIL_RUNNER_ADDR", ":9090")
	image := os.Getenv("ANVIL_SANDBOX_IMAGE")
	if image == "" {
		return fmt.Errorf("run: ANVIL_SANDBOX_IMAGE is required")
	}

	server, err := runner.New(runner.Config{
		Addr:        addr,
		Logger:      log,
		Image:       image,
		MaxLifetime: 30 * time.Minute,
		ExecTimeout: 300 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("run: construct server: %w", err)
	}

	log.Info("starting", slog.String("addr", addr), slog.String("image", image))
	if err := server.Run(ctx); err != nil {
		return fmt.Errorf("run: server: %w", err)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
