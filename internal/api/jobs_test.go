package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/queue"
)

func newJobsTestServer(t *testing.T, userID uuid.UUID, pub *fakePublisher) *Server {
	t.Helper()
	a := &fakeAuth{verifyFn: func(_ string) (uuid.UUID, error) { return userID, nil }}
	srv, err := New(Config{
		Addr:       ":0",
		Auth:       a,
		Store:      &fakePinger{},
		Pool:       testPool,
		Hub:        &fakeHub{},
		EventStore: &fakeEventStore{},
		Publisher:  pub,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return srv
}

func seedAPITestUser(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := testPool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		uuid.NewString()+"@example.com",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func authedRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer valid-token")
	return req
}

func TestJobs_HandleCreateJob_Returns202WithIDAndEventsURL(t *testing.T) {
	t.Parallel()
	userID := seedAPITestUser(t)
	pub := &fakePublisher{}
	srv := newJobsTestServer(t, userID, pub)

	req := authedRequest(http.MethodPost, "/v1/jobs", `{"prompt":"build me a thing"}`)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}

	var resp jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID == "" {
		t.Error("response has no id")
	}
	if resp.EventsURL == "" {
		t.Error("response has no events_url")
	}
	// The job starts in PENDING_PLAN: the Planner picks it up and lands
	// it in AWAITING_APPROVAL or, with options.auto_approve, QUEUED
	// (PRD §11, §13.1) — this request didn't set auto_approve.
	if resp.Status != string(queue.StatusPendingPlan) {
		t.Errorf("Status = %q, want %q", resp.Status, queue.StatusPendingPlan)
	}

	if len(pub.events) != 1 || pub.events[0] != "job_created" {
		t.Errorf("published events = %v, want exactly one job_created", pub.events)
	}
}

func TestJobs_HandleCreateJob_RejectsEmptyPrompt(t *testing.T) {
	t.Parallel()
	srv := newJobsTestServer(t, seedAPITestUser(t), &fakePublisher{})

	req := authedRequest(http.MethodPost, "/v1/jobs", `{"prompt":""}`)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestJobs_HandleCreateJob_RequiresAuth(t *testing.T) {
	t.Parallel()
	srv := newJobsTestServer(t, seedAPITestUser(t), &fakePublisher{})

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"prompt":"x"}`))
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestJobs_HandleGetJob_NotFoundForAnotherUsersJob(t *testing.T) {
	t.Parallel()
	owner := seedAPITestUser(t)
	other := seedAPITestUser(t)

	job, err := queue.CreateQueuedJob(context.Background(), testPool, owner, "owner's job")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	srv := newJobsTestServer(t, other, &fakePublisher{})
	req := authedRequest(http.MethodGet, "/v1/jobs/"+job.ID.String(), "")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — a job must not be visible to a caller who doesn't own it", rec.Code)
	}
}

func TestJobs_HandleGetJob_ReturnsOwnJob(t *testing.T) {
	t.Parallel()
	userID := seedAPITestUser(t)
	job, err := queue.CreateQueuedJob(context.Background(), testPool, userID, "my job")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	srv := newJobsTestServer(t, userID, &fakePublisher{})
	req := authedRequest(http.MethodGet, "/v1/jobs/"+job.ID.String(), "")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != job.ID.String() {
		t.Errorf("ID = %q, want %q", resp.ID, job.ID)
	}
}

func TestJobs_HandleListJobs_ReturnsOnlyCallersJobs(t *testing.T) {
	t.Parallel()
	userID := seedAPITestUser(t)
	other := seedAPITestUser(t)

	if _, err := queue.CreateQueuedJob(context.Background(), testPool, userID, "mine"); err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}
	if _, err := queue.CreateQueuedJob(context.Background(), testPool, other, "not mine"); err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	srv := newJobsTestServer(t, userID, &fakePublisher{})
	req := authedRequest(http.MethodGet, "/v1/jobs", "")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp []jobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp) != 1 || resp[0].Prompt != "mine" {
		t.Errorf("response = %+v, want exactly the caller's one job", resp)
	}
}

// TestJobs_HandleApproveJob_TransitionsToQueued is US-02: approval is a
// state transition, not just a UI acknowledgment.
func TestJobs_HandleApproveJob_TransitionsToQueued(t *testing.T) {
	t.Parallel()
	userID := seedAPITestUser(t)
	job, err := queue.CreateJob(context.Background(), testPool, userID, "my job", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	seedClaimedPlanningJob(t, job.ID)
	if err := queue.SavePlan(context.Background(), testPool, job.ID, "s", nil, []queue.PlannedStep{{Title: "t", Description: "d", Acceptance: "a"}}, false); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	srv := newJobsTestServer(t, userID, &fakePublisher{})
	req := authedRequest(http.MethodPost, "/v1/jobs/"+job.ID.String()+"/approve", "")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}

	got, err := queue.GetJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != queue.StatusQueued {
		t.Errorf("Status = %s, want QUEUED", got.Status)
	}
}

// TestJobs_HandleApproveJob_RejectsWrongState proves the state machine,
// not the frontend, blocks approving a job that isn't AWAITING_APPROVAL
// (INV-4).
func TestJobs_HandleApproveJob_RejectsWrongState(t *testing.T) {
	t.Parallel()
	userID := seedAPITestUser(t)
	job, err := queue.CreateJob(context.Background(), testPool, userID, "my job", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	srv := newJobsTestServer(t, userID, &fakePublisher{})
	req := authedRequest(http.MethodPost, "/v1/jobs/"+job.ID.String()+"/approve", "")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 for a job still in PENDING_PLAN", rec.Code)
	}
}

// TestJobs_HandleCancelJob_SetsCancelRequested is PRD §13.3 step 1.
func TestJobs_HandleCancelJob_SetsCancelRequested(t *testing.T) {
	t.Parallel()
	userID := seedAPITestUser(t)
	job, err := queue.CreateQueuedJob(context.Background(), testPool, userID, "my job")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	srv := newJobsTestServer(t, userID, &fakePublisher{})
	req := authedRequest(http.MethodPost, "/v1/jobs/"+job.ID.String()+"/cancel", "")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}

	requested, err := queue.IsCancelRequested(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("IsCancelRequested: %v", err)
	}
	if !requested {
		t.Error("cancel_requested_at was not set")
	}
}

// TestJobs_HandleCancelJob_NotFoundForAnotherUsersJob proves cancel
// respects ownership the same way every other job endpoint does.
func TestJobs_HandleCancelJob_NotFoundForAnotherUsersJob(t *testing.T) {
	t.Parallel()
	owner := seedAPITestUser(t)
	caller := seedAPITestUser(t)
	job, err := queue.CreateQueuedJob(context.Background(), testPool, owner, "not mine")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	srv := newJobsTestServer(t, caller, &fakePublisher{})
	req := authedRequest(http.MethodPost, "/v1/jobs/"+job.ID.String()+"/cancel", "")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// seedClaimedPlanningJob moves job (already PENDING_PLAN via CreateJob)
// into PLANNING with a lease, directly — the state SavePlan expects to
// transition out of, without needing a real Planner run in this test.
func seedClaimedPlanningJob(t *testing.T, jobID uuid.UUID) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`UPDATE jobs SET status = 'PLANNING', lease_owner = 'test', lease_expires_at = now() + interval '1 minute' WHERE id = $1`,
		jobID)
	if err != nil {
		t.Fatalf("seedClaimedPlanningJob: %v", err)
	}
}
