package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anvil-dev/anvil/internal/auth"
)

const shutdownGracePeriod = 30 * time.Second

// authService is the subset of auth the API layer needs.
type authService interface {
	Register(ctx context.Context, email, password string) (auth.TokenPair, error)
	Login(ctx context.Context, email, password string) (auth.TokenPair, error)
	Refresh(ctx context.Context, refreshToken string) (auth.TokenPair, error)
	Logout(ctx context.Context, callerID uuid.UUID, refreshToken string) error
	VerifyAccessToken(token string) (uuid.UUID, error)
}

// Config configures a Server.
type Config struct {
	Addr       string
	Auth       authService
	Store      pinger
	Pool       *pgxpool.Pool // jobs and steps live in queue's tables, read/written directly
	Hub        eventHub
	EventStore eventStore
	Publisher  eventPublisher
	Logger     *slog.Logger
}

func (c Config) validate() error {
	if c.Addr == "" {
		return errors.New("api: config: Addr is required")
	}
	if c.Auth == nil {
		return errors.New("api: config: Auth is required")
	}
	if c.Store == nil {
		return errors.New("api: config: Store is required")
	}
	if c.Pool == nil {
		return errors.New("api: config: Pool is required")
	}
	if c.Hub == nil {
		return errors.New("api: config: Hub is required")
	}
	if c.EventStore == nil {
		return errors.New("api: config: EventStore is required")
	}
	if c.Publisher == nil {
		return errors.New("api: config: Publisher is required")
	}
	if c.Logger == nil {
		return errors.New("api: config: Logger is required")
	}
	return nil
}

// Server is the HTTP + SSE edge (PRD §9.1). It terminates HTTP,
// authenticates, authorizes, and validates, and contains zero business
// logic.
type Server struct {
	httpServer *http.Server
	auth       authService
	store      pinger
	pool       *pgxpool.Pool
	hub        eventHub
	eventStore eventStore
	publisher  eventPublisher
	log        *slog.Logger
}

// New constructs a Server from cfg, or returns an error if cfg is invalid.
func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	s := &Server{
		auth:       cfg.Auth,
		store:      cfg.Store,
		pool:       cfg.Pool,
		hub:        cfg.Hub,
		eventStore: cfg.EventStore,
		publisher:  cfg.Publisher,
		log:        cfg.Logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/register", s.handleRegister)
	mux.HandleFunc("POST /auth/login", s.handleLogin)
	mux.HandleFunc("POST /auth/refresh", s.handleAuthRefresh)
	mux.Handle("POST /auth/logout", requireAuth(cfg.Auth)(http.HandlerFunc(s.handleLogout)))
	mux.Handle("POST /v1/jobs", requireAuth(cfg.Auth)(http.HandlerFunc(s.handleCreateJob)))
	mux.Handle("GET /v1/jobs", requireAuth(cfg.Auth)(http.HandlerFunc(s.handleListJobs)))
	mux.Handle("GET /v1/jobs/{id}", requireAuth(cfg.Auth)(http.HandlerFunc(s.handleGetJob)))
	mux.Handle("POST /v1/jobs/{id}/approve", requireAuth(cfg.Auth)(http.HandlerFunc(s.handleApproveJob)))
	mux.Handle("POST /v1/jobs/{id}/cancel", requireAuth(cfg.Auth)(http.HandlerFunc(s.handleCancelJob)))
	mux.Handle("GET /v1/jobs/{id}/events", requireAuthSSE(cfg.Auth)(http.HandlerFunc(s.handleJobEvents)))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	handler := cors(traceID(recoverPanic(cfg.Logger)(mux)))

	s.httpServer = &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,
	}
	return s, nil
}

// Run starts serving and blocks until ctx is cancelled, then drains
// in-flight requests and returns. Owns the HTTP listener goroutine; no
// goroutine started by Run outlives Run's return (CLAUDE.md I-5).
func (s *Server) Run(ctx context.Context) error {
	serveErr := make(chan error, 1)

	// Listener goroutine: owned by Run, exits when ListenAndServe returns
	// (either an error, or http.ErrServerClosed from the Shutdown call
	// below). Run does not return until this goroutine has sent to
	// serveErr, so it never outlives Run (CLAUDE.md I-5).
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("api: serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		// Deliberately not derived from ctx: we're in this branch because
		// ctx is already Done, so a child of it would expire immediately
		// and Shutdown would get no grace period at all.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil { //nolint:contextcheck // reason: shutdownCtx is intentionally rooted at context.Background(), not ctx — see comment above
			return fmt.Errorf("api: graceful shutdown: %w", err)
		}
		<-serveErr
		return nil
	}
}
