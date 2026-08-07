package api

import (
	"context"
	"net/http"
)

// pinger is the subset of storage the readiness check needs.
type pinger interface {
	Ping(ctx context.Context) error
}

// handleHealthz is a liveness check independent of dependencies (FR-006).
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handleReadyz reports readiness by checking database reachability.
// Redis reachability joins this check once internal/events introduces a
// Redis client (Week 4) — see specs/phase-1-skeleton/week-01-foundations.md.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
