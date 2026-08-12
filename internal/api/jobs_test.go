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
	if resp.Status != string(queue.StatusQueued) {
		t.Errorf("Status = %q, want %q", resp.Status, queue.StatusQueued)
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
