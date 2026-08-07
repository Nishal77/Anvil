package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	dockerclient "github.com/moby/moby/client"
)

const shutdownGracePeriod = 30 * time.Second

// Config configures a Server.
type Config struct {
	Addr        string
	Logger      *slog.Logger
	Image       string        // pinned workspace image, referenced by digest
	MaxLifetime time.Duration // default 30m — sandboxes older than this get destroyed automatically
	ExecTimeout time.Duration // default 300s — how long a single command is allowed to run
}

func (c *Config) setDefaults() {
	if c.MaxLifetime <= 0 {
		c.MaxLifetime = 30 * time.Minute
	}
	if c.ExecTimeout <= 0 {
		c.ExecTimeout = 300 * time.Second
	}
}

func (c Config) validate() error {
	if c.Addr == "" {
		return errors.New("runner: config: Addr is required")
	}
	if c.Logger == nil {
		return errors.New("runner: config: Logger is required")
	}
	if c.Image == "" {
		return errors.New("runner: config: Image is required")
	}
	return nil
}

// Server implements the sandbox protocol's HTTP handlers and owns the
// Docker client it builds internally — nothing outside this package ever
// holds or passes in a Docker client directly.
type Server struct {
	httpServer  *http.Server
	docker      *dockerclient.Client
	log         *slog.Logger
	image       string
	maxLifetime time.Duration
	execTimeout time.Duration

	// mu guards containers. Held only for map access, never across a
	// Docker API call — a slow create/destroy must not block every other
	// request.
	mu         sync.Mutex
	containers map[string]time.Time // sandboxID -> created at
}

// New constructs a Server, including its own Docker client.
func New(cfg Config) (*Server, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	docker, err := dockerclient.New(dockerclient.FromEnv) // API version negotiation is on by default
	if err != nil {
		return nil, fmt.Errorf("runner: construct docker client: %w", err)
	}

	s := &Server{
		docker:      docker,
		log:         cfg.Logger,
		image:       cfg.Image,
		maxLifetime: cfg.MaxLifetime,
		execTimeout: cfg.ExecTimeout,
		containers:  make(map[string]time.Time),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", s.handleCreate)
	mux.HandleFunc("POST /sandboxes/{id}/exec", s.handleExec)
	mux.HandleFunc("DELETE /sandboxes/{id}", s.handleDestroy)

	s.httpServer = &http.Server{Addr: cfg.Addr, Handler: mux}
	return s, nil
}

// Run makes sure the sandbox network exists, then starts serving requests
// and the background cleanup loop, blocking until ctx is cancelled. On
// shutdown, it destroys every container it's still tracking before
// returning, so a clean shutdown never leaves containers running.
func (s *Server) Run(ctx context.Context) error {
	if err := ensureSandboxNetwork(ctx, s.docker); err != nil {
		return fmt.Errorf("runner: ensure sandbox network: %w", err)
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	reapDone := make(chan struct{})
	go func() {
		defer close(reapDone)
		s.reapLoop(ctx)
	}()

	select {
	case err := <-serveErr:
		<-reapDone
		if closeErr := s.docker.Close(); closeErr != nil {
			s.log.Error("docker client close failed", slog.Any("err", closeErr))
		}
		if err != nil {
			return fmt.Errorf("runner: serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil { //nolint:contextcheck // reason: shutdownCtx is intentionally rooted at context.Background(), not ctx, which is already cancelled here
			s.log.Error("http shutdown failed", slog.Any("err", err))
		}
		<-reapDone
		<-serveErr
		s.destroyAllTracked(shutdownCtx) //nolint:contextcheck // reason: shutdownCtx is intentionally rooted at context.Background(), not ctx, which is already cancelled here
		// The Docker client keeps its own background goroutines alive for
		// connection reuse — closing it here is the last thing Run needs
		// to do before it returns, so nothing gets left running.
		if err := s.docker.Close(); err != nil {
			s.log.Error("docker client close failed", slog.Any("err", err))
		}
		return nil
	}
}

func (s *Server) trackContainer(id string) {
	s.mu.Lock()
	s.containers[id] = time.Now()
	s.mu.Unlock()
}

func (s *Server) untrackContainer(id string) {
	s.mu.Lock()
	delete(s.containers, id)
	s.mu.Unlock()
}

func (s *Server) destroyAllTracked(ctx context.Context) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.containers))
	for id := range s.containers {
		ids = append(ids, id)
	}
	s.mu.Unlock()

	for _, id := range ids {
		if err := destroyContainer(ctx, s.docker, id); err != nil {
			s.log.Error("destroy on shutdown failed", slog.String("sandbox_id", id), slog.Any("err", err))
			continue
		}
		s.untrackContainer(id)
	}
}
