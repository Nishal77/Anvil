package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
)

func newEventsTestServer(t *testing.T, userID uuid.UUID, hub *fakeHub, es *fakeEventStore) *Server {
	t.Helper()
	a := &fakeAuth{verifyFn: func(_ string) (uuid.UUID, error) { return userID, nil }}
	srv, err := New(Config{
		Addr:       ":0",
		Auth:       a,
		Store:      &fakePinger{},
		Pool:       testPool,
		Hub:        hub,
		EventStore: es,
		Publisher:  &fakePublisher{},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return srv
}

// flushRecorder wraps httptest.ResponseRecorder to notify flushed on
// every Flush call — the SSE handler flushes after every event and
// heartbeat it writes, so a test can wait on that channel instead of
// polling the response body on a timer. It also guards the body with its
// own mutex: the handler writes from its own goroutine while the test
// goroutine reads, and httptest.ResponseRecorder's embedded buffer isn't
// safe for that on its own.
type flushRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	flushed chan struct{}
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 1)}
}

func (r *flushRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.ResponseRecorder.Write(p)
	if err != nil {
		return n, fmt.Errorf("flushRecorder: write: %w", err)
	}
	return n, nil
}

func (r *flushRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ResponseRecorder.WriteHeader(status)
}

func (r *flushRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Body.String()
}

func (r *flushRecorder) Flush() {
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}

// serveSSE runs the SSE handler against req in its own goroutine, since
// it blocks streaming until the request's context is cancelled — exactly
// like a real client closing its connection.
func serveSSE(srv *Server, req *http.Request) (*flushRecorder, context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := newFlushRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.httpServer.Handler.ServeHTTP(rec, req)
	}()
	return rec, cancel, done
}

func TestFR053_SSEEventIDEqualsSeq(t *testing.T) {
	t.Parallel()
	userID := seedAPITestUser(t)
	job, err := queue.CreateQueuedJob(context.Background(), testPool, userID, "x")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	es := &fakeEventStore{}
	es.seed(job.ID, storage.Event{Seq: 1, Type: "job_created", Payload: json.RawMessage(`{}`)})
	hub := &fakeHub{}
	srv := newEventsTestServer(t, userID, hub, es)

	req := authedRequest(http.MethodGet, "/v1/jobs/"+job.ID.String()+"/events", "")
	rec, cancel, done := serveSSE(srv, req)
	defer func() { cancel(); <-done }()

	waitForBody(t, rec, "id: 1")
	if !strings.Contains(rec.body(), "event: job_created") {
		t.Errorf("body = %q, want an event: job_created line", rec.body())
	}
}

func TestJoin_ClosedSSEConnectionReplaysOnReconnect(t *testing.T) {
	t.Parallel()
	userID := seedAPITestUser(t)
	job, err := queue.CreateQueuedJob(context.Background(), testPool, userID, "x")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	es := &fakeEventStore{}
	es.seed(job.ID,
		storage.Event{Seq: 1, Type: "job_created", Payload: json.RawMessage(`{}`)},
		storage.Event{Seq: 2, Type: "step_started", Payload: json.RawMessage(`{}`)},
		storage.Event{Seq: 3, Type: "step_finished", Payload: json.RawMessage(`{}`)},
	)
	hub := &fakeHub{}
	srv := newEventsTestServer(t, userID, hub, es)

	// First connection, from the start.
	req1 := authedRequest(http.MethodGet, "/v1/jobs/"+job.ID.String()+"/events", "")
	rec1, cancel1, done1 := serveSSE(srv, req1)
	waitForBody(t, rec1, "id: 3")
	cancel1()
	<-done1

	// Reconnect with Last-Event-ID set to what the "browser" last saw —
	// only the events after that should replay.
	req2 := authedRequest(http.MethodGet, "/v1/jobs/"+job.ID.String()+"/events", "")
	req2.Header.Set("Last-Event-ID", "1")
	rec2, cancel2, done2 := serveSSE(srv, req2)
	defer func() { cancel2(); <-done2 }()
	waitForBody(t, rec2, "id: 3")

	body := rec2.body()
	if strings.Contains(body, "id: 1\n") {
		t.Errorf("reconnect body = %q, must not replay seq 1 again (already seen)", body)
	}
	if !strings.Contains(body, "id: 2") || !strings.Contains(body, "id: 3") {
		t.Errorf("reconnect body = %q, want seq 2 and 3 replayed", body)
	}
}

func TestJobs_HandleJobEvents_AcceptsTokenAsQueryParam(t *testing.T) {
	t.Parallel()
	userID := seedAPITestUser(t)
	job, err := queue.CreateQueuedJob(context.Background(), testPool, userID, "x")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	es := &fakeEventStore{}
	es.seed(job.ID, storage.Event{Seq: 1, Type: "job_created", Payload: json.RawMessage(`{}`)})
	srv := newEventsTestServer(t, userID, &fakeHub{}, es)

	// No Authorization header at all — a native browser EventSource can't
	// set one, so this route must also accept the token via ?access_token=.
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+job.ID.String()+"/events?access_token=valid-token", nil)
	rec, cancel, done := serveSSE(srv, req)
	defer func() { cancel(); <-done }()

	waitForBody(t, rec, "id: 1")
}

func TestJobs_HandleJobEvents_NotFoundForAnotherUsersJob(t *testing.T) {
	t.Parallel()
	owner := seedAPITestUser(t)
	other := seedAPITestUser(t)
	job, err := queue.CreateQueuedJob(context.Background(), testPool, owner, "x")
	if err != nil {
		t.Fatalf("CreateQueuedJob: %v", err)
	}

	srv := newEventsTestServer(t, other, &fakeHub{}, &fakeEventStore{})
	req := authedRequest(http.MethodGet, "/v1/jobs/"+job.ID.String()+"/events", "")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// waitForBody waits for rec's body to contain want, waking up only when
// the handler actually flushes — never on a timer — up to an overall
// deadline in case want never arrives at all.
func waitForBody(t *testing.T, rec *flushRecorder, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(rec.body(), want) {
			return
		}
		select {
		case <-rec.flushed:
		case <-deadline:
			t.Fatalf("body never contained %q; got %q", want, rec.body())
		}
	}
}
