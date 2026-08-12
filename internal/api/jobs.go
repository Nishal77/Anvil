package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
)

// eventPublisher is the subset of events.Publisher the jobs handlers need.
type eventPublisher interface {
	Publish(ctx context.Context, jobID uuid.UUID, typ storage.EventType, payload json.RawMessage) error
}

const (
	defaultJobsPageSize = 20
	maxJobsPageSize     = 100
)

// parsePagination reads ?limit=&offset= from r, applying a default and a
// cap on limit so a caller can't force an unbounded query.
func parsePagination(r *http.Request) (limit, offset int) {
	limit = defaultJobsPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= maxJobsPageSize {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

const maxPromptBytes = 8000

type createJobRequest struct {
	Prompt string `json:"prompt"`
}

func (r createJobRequest) validate() error {
	if r.Prompt == "" {
		return errors.New("prompt is required")
	}
	if len(r.Prompt) > maxPromptBytes {
		return errors.New("prompt must be at most 8000 characters")
	}
	return nil
}

type jobResponse struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Prompt    string `json:"prompt"`
	EventsURL string `json:"events_url,omitempty"`
}

func toJobResponse(j *queue.Job, withEventsURL bool) jobResponse {
	resp := jobResponse{ID: j.ID.String(), Status: string(j.Status), Prompt: j.Prompt}
	if withEventsURL {
		resp.EventsURL = "/v1/jobs/" + j.ID.String() + "/events"
	}
	return resp
}

// handleCreateJob — POST /v1/jobs. Phase 1 has no planner: the job goes
// straight to QUEUED with a fixed plan (internal/executor), so there's no
// plan to approve either.
func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://anvil.dev/errors/invalid-request", "Invalid request", err.Error())
		return
	}

	job, err := queue.CreateQueuedJob(r.Context(), s.pool, authenticatedUserID(r.Context()), req.Prompt)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	if payload, err := json.Marshal(map[string]any{"prompt": job.Prompt}); err == nil {
		if err := s.publisher.Publish(r.Context(), job.ID, "job_created", payload); err != nil {
			s.log.ErrorContext(r.Context(), "publish job_created failed", "job_id", job.ID, "err", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(toJobResponse(job, true))
}

// handleListJobs — GET /v1/jobs. Paginated with ?limit=&offset=, both
// optional.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePagination(r)

	jobs, err := queue.ListJobsForUser(r.Context(), s.pool, authenticatedUserID(r.Context()), limit, offset)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	resp := make([]jobResponse, len(jobs))
	for i, j := range jobs {
		resp[i] = toJobResponse(j, false)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleGetJob — GET /v1/jobs/{id}. 404s a job that doesn't exist or
// doesn't belong to the caller — the same response either way, so a
// caller can't use this to probe which job IDs exist.
func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}

	job, err := queue.GetJob(r.Context(), s.pool, jobID)
	if err != nil {
		if errors.Is(err, queue.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "https://anvil.dev/errors/not-found", "Job not found", "")
			return
		}
		s.writeInternalError(w, r, err)
		return
	}
	if job.UserID != authenticatedUserID(r.Context()) {
		// Same response as a genuinely missing job — a caller can't use
		// this to tell "not mine" apart from "doesn't exist."
		writeProblem(w, r, http.StatusNotFound, "https://anvil.dev/errors/not-found", "Job not found", "")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toJobResponse(job, true))
}

func parseJobID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://anvil.dev/errors/invalid-request", "Invalid job id", err.Error())
		return uuid.UUID{}, false
	}
	return id, true
}
