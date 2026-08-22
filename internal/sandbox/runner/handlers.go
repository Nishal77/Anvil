package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/anvil-dev/anvil/internal/sandbox"
	"github.com/anvil-dev/anvil/internal/telemetry"
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

	exitCode, err := s.runContainerSpan(r.Context(), sandboxID, req.Command, timeout, onChunk)
	final := sandbox.ExecChunk{Final: true, ExitCode: exitCode, Timestamp: time.Now()}
	if err != nil && !errors.Is(err, sandbox.ErrCommandTimeout) {
		s.log.ErrorContext(r.Context(), "exec failed", slog.String("sandbox_id", sandboxID), slog.Any("err", err))
	}
	_ = enc.Encode(final)
	flusher.Flush()
}

// runContainerSpan wraps runInGroup in the "container.run" span PRD
// §17.1's tree nests under sandbox.exec — the leaf that actually runs a
// command inside the container, ending exactly when the process exits.
// Split out of handleExec so the streaming response-writing logic there
// stays untouched by (and untouched *by*) span bookkeeping — no part of
// this function or what it calls ever holds w or a Flusher, unlike
// telemetry.WrapHandler, which this handler deliberately doesn't use.
func (s *Server) runContainerSpan(ctx context.Context, sandboxID, command string, timeout time.Duration, onChunk func(string, []byte)) (int, error) {
	ctx, span := telemetry.Tracer("runner").Start(ctx, "container.run", trace.WithAttributes(
		attribute.String("anvil.command", command),
	))
	defer span.End()

	exitCode, err := runInGroup(ctx, s.docker, sandboxID, command, timeout, onChunk)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return exitCode, err
}

// handleWriteFile — POST /sandboxes/{id}/write. Writes req.Data to
// req.Path inside the sandbox via a short-lived `cat > path` exec —
// the control-plane side of SEC-020's named-pipe credential injection
// (see sandbox.WriteRequest's doc comment).
func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("id")

	var req sandbox.WriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}

	if err := writeStdin(r.Context(), s.docker, sandboxID, req.Path, req.Data); err != nil {
		s.log.ErrorContext(r.Context(), "write stdin failed", slog.String("sandbox_id", sandboxID), slog.Any("err", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// previewBuildTimeout bounds how long a preview's image build and
// startup may take — generous, since a real project's dependency
// install (npm ci, go mod download) is the dominant cost, not the
// container start itself.
const previewBuildTimeout = 5 * time.Minute

// handleBuildPreview — POST /previews/{job_id}. The request body is
// the raw build context (a tar or tar.gz stream, Dockerfile
// guaranteed present by internal/deploy — task 9.3) for
// docker.ImageBuild, not a JSON envelope: previews are one-shot
// image-build-then-run operations, not a stream of chunks, so there's
// nothing the JSON request types elsewhere in this package would add.
func (s *Server) handleBuildPreview(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")

	ctx, cancel := context.WithTimeout(r.Context(), previewBuildTimeout)
	defer cancel()

	if err := buildPreviewImage(ctx, s.docker, jobID, r.Body); err != nil {
		s.log.ErrorContext(r.Context(), "build preview image failed", slog.String("job_id", jobID), slog.Any("err", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	containerID, hostPort, err := runPreviewContainer(ctx, s.docker, jobID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "run preview container failed", slog.String("job_id", jobID), slog.Any("err", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.trackPreview(jobID, containerID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(sandbox.BuildPreviewResponse{ContainerID: containerID, HostPort: hostPort})
}

// handleDestroyPreview — DELETE /previews/{job_id}. Idempotent: a job
// with no tracked preview (never deployed, or already torn down) is
// treated the same as a successful destroy.
func (s *Server) handleDestroyPreview(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")

	containerID, ok := s.lookupPreview(jobID)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := destroyPreview(r.Context(), s.docker, jobID, containerID); err != nil {
		s.log.ErrorContext(r.Context(), "destroy preview failed", slog.String("job_id", jobID), slog.Any("err", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.untrackPreview(jobID)
	w.WriteHeader(http.StatusNoContent)
}
