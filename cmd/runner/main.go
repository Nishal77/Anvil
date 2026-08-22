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

// shutdownTracingTimeout bounds the final trace flush on exit.
const shutdownTracingTimeout = 5 * time.Second

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

	shutdownTracing, err := telemetry.NewTracerProvider(ctx, telemetry.TracerConfig{
		ServiceName:       "anvil-runner",
		CollectorEndpoint: os.Getenv("ANVIL_OTEL_COLLECTOR_ENDPOINT"),
	})
	if err != nil {
		return fmt.Errorf("run: construct tracer provider: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTracingTimeout)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			log.Error("shut down tracer provider", slog.Any("err", err))
		}
	}()

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
