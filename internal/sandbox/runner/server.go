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
	PreviewTTL  time.Duration // default 2h (FR-063) — previews older than this get destroyed automatically
}

func (c *Config) setDefaults() {
	if c.MaxLifetime <= 0 {
		c.MaxLifetime = 30 * time.Minute
	}
	if c.ExecTimeout <= 0 {
		c.ExecTimeout = 300 * time.Second
	}
	if c.PreviewTTL <= 0 {
		c.PreviewTTL = 2 * time.Hour
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

// previewInfo is what the Server tracks about one running preview
// deployment, keyed by job ID (the natural key a caller addresses a
// preview by — there is at most one live preview per job).
type previewInfo struct {
	containerID string
	createdAt   time.Time
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
	previewTTL  time.Duration

	// mu guards containers and previews. Held only for map access,
	// never across a Docker API call — a slow create/destroy must not
	// block every other request.
	mu         sync.Mutex
	containers map[string]time.Time   // sandboxID -> created at
	previews   map[string]previewInfo // jobID -> info
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
		previewTTL:  cfg.PreviewTTL,
		containers:  make(map[string]time.Time),
		previews:    make(map[string]previewInfo),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", s.handleCreate)
	mux.HandleFunc("POST /sandboxes/{id}/exec", s.handleExec)
	mux.HandleFunc("POST /sandboxes/{id}/write", s.handleWriteFile)
	mux.HandleFunc("DELETE /sandboxes/{id}", s.handleDestroy)
	mux.HandleFunc("POST /previews/{job_id}", s.handleBuildPreview)
	mux.HandleFunc("DELETE /previews/{job_id}", s.handleDestroyPreview)

	s.httpServer = &http.Server{Addr: cfg.Addr, Handler: mux}
	return s, nil
}

// Run makes sure the sandbox network exists, then starts serving requests
// and the background cleanup loop, blocking until ctx is cancelled. On
// shutdown, it destroys every container it's still tracking before
// returning, so a clean shutdown never leaves containers running.
func (s *Server) Run(ctx context.Context) error {
	if err := ensureNetworks(ctx, s.docker); err != nil {
		return err
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
	previews := make(map[string]previewInfo, len(s.previews))
	for jobID, info := range s.previews {
		previews[jobID] = info
	}
	s.mu.Unlock()

	for _, id := range ids {
		if err := destroyContainer(ctx, s.docker, id); err != nil {
			s.log.Error("destroy on shutdown failed", slog.String("sandbox_id", id), slog.Any("err", err))
			continue
		}
		s.untrackContainer(id)
	}
	for jobID, info := range previews {
		if err := destroyPreview(ctx, s.docker, jobID, info.containerID); err != nil {
			s.log.Error("destroy preview on shutdown failed", slog.String("job_id", jobID), slog.Any("err", err))
			continue
		}
		s.untrackPreview(jobID)
	}
}

func (s *Server) trackPreview(jobID, containerID string) {
	s.mu.Lock()
	s.previews[jobID] = previewInfo{containerID: containerID, createdAt: time.Now()}
	s.mu.Unlock()
}

func (s *Server) untrackPreview(jobID string) {
	s.mu.Lock()
	delete(s.previews, jobID)
	s.mu.Unlock()
}

// lookupPreview returns jobID's tracked container ID, or ok=false if
// no preview is tracked for it.
func (s *Server) lookupPreview(jobID string) (containerID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.previews[jobID]
	return info.containerID, ok
}
