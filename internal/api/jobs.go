package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
)

// eventPublisher is the subset of events.Publisher the jobs handlers need.
type eventPublisher interface {
	Publish(ctx context.Context, jobID uuid.UUID, typ storage.EventType, payload json.RawMessage) error
}

// artifactDownloader is the subset of *artifact.Store the jobs
// handlers need, declared at the consumer per CODE-STANDARDS §3.1.
type artifactDownloader interface {
	PresignedDownloadURL(ctx context.Context, jobID uuid.UUID, expiry time.Duration) (string, error)
}

// presignedDownloadExpiry bounds how long a GET /jobs/{id}/artifact
// redirect stays usable — long enough for a slow download to start,
// short enough that a leaked URL (browser history, a proxy log) isn't
// a standing credential.
const presignedDownloadExpiry = 15 * time.Minute

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

type createJobOptions struct {
	AutoApprove bool `json:"auto_approve"`
	CreateRepo  bool `json:"create_repo"`
	Deploy      bool `json:"deploy"`
}

type createJobRequest struct {
	Prompt  string           `json:"prompt"`
	Options createJobOptions `json:"options"`
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
	// FailureReason is empty unless Status is FAILED.
	FailureReason string `json:"failure_reason,omitempty"`
	// PlanSummary is empty until the planner finishes (PRD §12.1).
	PlanSummary string `json:"plan_summary,omitempty"`
	// PreviewURL is empty unless a deploy has completed (FR-060..FR-062).
	PreviewURL string `json:"preview_url,omitempty"`
	// HasArtifact tells the frontend whether GET .../artifact will
	// 404 — cheaper for a job list than following every job's
	// artifact link speculatively.
	HasArtifact bool `json:"has_artifact"`
	// Token spend and remaining budget (US-07). CostUSDMicros is USD
	// millionths — e.g. 61700 = $0.0617 — the same unit
	// internal/llm.CostUSDMicros already computes in, so the frontend
	// divides once rather than the API guessing a display precision.
	TokenBudget   int64 `json:"token_budget"`
	TokensUsed    int64 `json:"tokens_used"`
	CostUSDMicros int64 `json:"cost_usd_micros"`
}

func toJobResponse(j *queue.Job, withEventsURL bool) jobResponse {
	resp := jobResponse{
		ID: j.ID.String(), Status: string(j.Status), Prompt: j.Prompt,
		FailureReason: j.FailureReason, PlanSummary: j.PlanSummary, PreviewURL: j.PreviewURL,
		HasArtifact: j.ArtifactKey != "",
		TokenBudget: j.TokenBudget, TokensUsed: j.TokensUsed, CostUSDMicros: j.CostUSDMicros,
	}
	if withEventsURL {
		resp.EventsURL = "/v1/jobs/" + j.ID.String() + "/events"
	}
	return resp
}

// handleCreateJob — POST /v1/jobs. The job starts in PENDING_PLAN: the
// Planner (internal/agent) picks it up, decomposes it, and lands it in
// AWAITING_APPROVAL — or, with options.auto_approve, straight in
// QUEUED (PRD §11, §13.1).
func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://anvil.dev/errors/invalid-request", "Invalid request", err.Error())
		return
	}

	job, err := queue.CreateJob(r.Context(), s.pool, authenticatedUserID(r.Context()), req.Prompt, queue.JobOptions{
		AutoApprove: req.Options.AutoApprove,
		CreateRepo:  req.Options.CreateRepo,
		Deploy:      req.Options.Deploy,
	})
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
	job, ok := s.loadOwnedJob(w, r)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toJobResponse(job, true))
}

// handleApproveJob — POST /v1/jobs/{id}/approve (PRD §11.2, US-02).
// Transitions AWAITING_APPROVAL -> QUEUED. The transition itself
// (queue.ApproveJob -> Transition) is the enforcement point: a job in
// any other status is rejected with IllegalTransitionError, so approval
// is a backend invariant, not something the frontend can be relied on
// to gate (INV-4).
func (s *Server) handleApproveJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.loadOwnedJob(w, r)
	if !ok {
		return
	}

	if err := queue.ApproveJob(r.Context(), s.pool, job.ID); err != nil {
		var illegal *queue.IllegalTransitionError
		if errors.As(err, &illegal) {
			writeProblem(w, r, http.StatusConflict, "https://anvil.dev/errors/invalid-state", "Job cannot be approved from its current status", err.Error())
			return
		}
		s.writeInternalError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleCancelJob — POST /v1/jobs/{id}/cancel (PRD §11.2, §13.3 step
// 1). Records the request; it does not itself change jobs.status — the
// executor (steps 2-4) or the sweeper's wedged-worker path (step 5)
// does that, once the job actually stops. Idempotent: cancelling an
// already-cancelling job is a no-op, and a caller can call this any
// number of times without side effects beyond the first.
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.loadOwnedJob(w, r)
	if !ok {
		return
	}

	if err := queue.RequestCancel(r.Context(), s.pool, job.ID); err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleRetryJob — POST /v1/jobs/{id}/retry (PRD §11.2, US-08).
// Transitions FAILED -> QUEUED so the dispatcher claims it again;
// RunStep's own resume logic picks up from the first step that never
// finished successfully. Like handleApproveJob, the transition itself
// is the enforcement point — a job in any other status, or one with
// no failed step to resume from, is rejected, never silently ignored.
func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.loadOwnedJob(w, r)
	if !ok {
		return
	}

	if err := queue.RetryJob(r.Context(), s.pool, job.ID); err != nil {
		var illegal *queue.IllegalTransitionError
		switch {
		case errors.As(err, &illegal):
			writeProblem(w, r, http.StatusConflict, "https://anvil.dev/errors/invalid-state", "Job cannot be retried from its current status", err.Error())
		case errors.Is(err, queue.ErrNoStepsToRetry):
			writeProblem(w, r, http.StatusConflict, "https://anvil.dev/errors/no-steps-to-retry", "Job has no failed step to retry from", err.Error())
		default:
			s.writeInternalError(w, r, err)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleDownloadArtifact — GET /v1/jobs/{id}/artifact. Redirects to a
// presigned URL for the job's uploaded workspace archive (a tar of
// /workspace as it stood when the job reached a terminal state —
// SUCCEEDED, FAILED, or CANCELLED all upload one, ADR-012), so the
// transfer itself bypasses the control plane (PRD §11.2). 503 if no
// artifact store is configured; 404 if this job has none (still
// running, or the upload itself failed — best-effort, PRD §13.3/§12.4).
func (s *Server) handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://anvil.dev/errors/not-configured", "Artifact storage not configured", "")
		return
	}
	job, ok := s.loadOwnedJob(w, r)
	if !ok {
		return
	}
	if job.ArtifactKey == "" {
		writeProblem(w, r, http.StatusNotFound, "https://anvil.dev/errors/not-found", "No artifact for this job", "")
		return
	}

	url, err := s.artifacts.PresignedDownloadURL(r.Context(), job.ID, presignedDownloadExpiry)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// loadOwnedJob reads the job named by {id} and 404s if it doesn't
// exist or doesn't belong to the caller — the same response either
// way, so a caller can't use this to probe which job IDs exist.
func (s *Server) loadOwnedJob(w http.ResponseWriter, r *http.Request) (*queue.Job, bool) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return nil, false
	}

	job, err := queue.GetJob(r.Context(), s.pool, jobID)
	if err != nil {
		if errors.Is(err, queue.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "https://anvil.dev/errors/not-found", "Job not found", "")
			return nil, false
		}
		s.writeInternalError(w, r, err)
		return nil, false
	}
	if job.UserID != authenticatedUserID(r.Context()) {
		writeProblem(w, r, http.StatusNotFound, "https://anvil.dev/errors/not-found", "Job not found", "")
		return nil, false
	}
	return job, true
}

func parseJobID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://anvil.dev/errors/invalid-request", "Invalid job id", err.Error())
		return uuid.UUID{}, false
	}
	return id, true
}
