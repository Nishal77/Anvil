package runner

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/anvil-dev/anvil/internal/sandbox"
)

// handleCreate — POST /sandboxes.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req sandbox.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}

	containerID, err := createContainer(r.Context(), s.docker, s.image)
	if err != nil {
		s.log.ErrorContext(r.Context(), "create container failed", slog.String("job_id", req.JobID.String()), slog.Any("err", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.trackContainer(containerID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(sandbox.CreateResponse{SandboxID: containerID})
}

// handleExec — POST /sandboxes/{id}/exec. Streams the command's output
// back one JSON line per chunk — each line is sent the moment it's ready,
// never held back until the command finishes.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("id")

	var req sandbox.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}

	timeout := s.execTimeout
	if req.TimeoutS > 0 {
		timeout = time.Duration(req.TimeoutS) * time.Second
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	// Flush the header immediately: a silent command (e.g. "sleep 60")
	// produces no output to piggyback the first flush on, and the client's
	// Do() call blocks until response headers actually reach the wire —
	// not until WriteHeader is called locally.
	flusher.Flush()
	enc := json.NewEncoder(w)

	// stdout and stderr are read by two separate goroutines, and both call
	// onChunk. Without this lock they could both write to the response at
	// the same time and scramble the output together.
	var writeMu sync.Mutex
	onChunk := func(stream string, data []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = enc.Encode(sandbox.ExecChunk{Stream: stream, Data: data, Timestamp: time.Now()})
		flusher.Flush()
	}

	exitCode, err := runInGroup(r.Context(), s.docker, sandboxID, req.Command, timeout, onChunk)
	final := sandbox.ExecChunk{Final: true, ExitCode: exitCode, Timestamp: time.Now()}
	if err != nil && !errors.Is(err, sandbox.ErrCommandTimeout) {
		s.log.ErrorContext(r.Context(), "exec failed", slog.String("sandbox_id", sandboxID), slog.Any("err", err))
	}
	_ = enc.Encode(final)
	flusher.Flush()
}

// handleDestroy — DELETE /sandboxes/{id}. Idempotent.
func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("id")

	if err := destroyContainer(r.Context(), s.docker, sandboxID); err != nil {
		s.log.ErrorContext(r.Context(), "destroy container failed", slog.String("sandbox_id", sandboxID), slog.Any("err", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.untrackContainer(sandboxID)
	w.WriteHeader(http.StatusNoContent)
}
