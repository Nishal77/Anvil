package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/queue"
	"github.com/anvil-dev/anvil/internal/storage"
)

const sseHeartbeatInterval = 20 * time.Second

// eventHub is the subset of events.Hub the SSE handler needs.
type eventHub interface {
	Subscribe(jobID uuid.UUID) (<-chan storage.Event, func())
}

// eventStore is the subset of storage the SSE handler needs, to replay
// history on connect or resume.
type eventStore interface {
	ListEventsFrom(ctx context.Context, jobID uuid.UUID, fromSeq int64) ([]storage.Event, error)
}

// handleJobEvents — GET /v1/jobs/{id}/events. Never holds a database
// connection for the stream's lifetime: it reads the requested backlog
// once up front, then serves everything after that from the Hub's
// in-memory channel — a leaked pgx connection per open browser tab would
// exhaust the connection pool within an hour.
func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}
	if !s.authorizeJobAccess(w, r, jobID) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeInternalError(w, r, fmt.Errorf("response writer does not support flushing"))
		return
	}

	fromSeq := parseFromSeq(r)

	// Subscribe before reading the backlog, not after: anything published
	// in between would otherwise fall in the gap between "what the
	// backlog query saw" and "what the live subscription starts seeing."
	live, unsubscribe := s.hub.Subscribe(jobID)
	defer unsubscribe()

	backlog, err := s.eventStore.ListEventsFrom(r.Context(), jobID, fromSeq)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // don't let a reverse proxy buffer the stream
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	lastSeq := fromSeq
	for _, ev := range backlog {
		writeSSEEvent(w, ev)
		lastSeq = ev.Seq
	}
	flusher.Flush()

	streamLiveEvents(r, w, flusher, live, lastSeq)
}

// authorizeJobAccess writes a 404 and returns false unless jobID exists
// and belongs to the caller — the same response either way, so a caller
// can't use this to tell "not mine" apart from "doesn't exist."
func (s *Server) authorizeJobAccess(w http.ResponseWriter, r *http.Request, jobID uuid.UUID) bool {
	job, err := queue.GetJob(r.Context(), s.pool, jobID)
	if err != nil {
		if errors.Is(err, queue.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "https://anvil.dev/errors/not-found", "Job not found", "")
			return false
		}
		s.writeInternalError(w, r, err)
		return false
	}
	if job.UserID != authenticatedUserID(r.Context()) {
		writeProblem(w, r, http.StatusNotFound, "https://anvil.dev/errors/not-found", "Job not found", "")
		return false
	}
	return true
}

// streamLiveEvents writes events from live as they arrive, skipping any
// already covered by the backlog, plus a heartbeat comment every
// sseHeartbeatInterval, until the request's context is done or live
// closes.
func streamLiveEvents(r *http.Request, w http.ResponseWriter, flusher http.Flusher, live <-chan storage.Event, lastSeq int64) {
	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case ev, ok := <-live:
			if !ok {
				return
			}
			if ev.Seq <= lastSeq {
				// Already delivered via the backlog query above.
				continue
			}
			writeSSEEvent(w, ev)
			lastSeq = ev.Seq
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, ev storage.Event) {
	_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, ev.Payload)
}

func parseFromSeq(r *http.Request) int64 {
	// The browser sends Last-Event-ID automatically on reconnect, once
	// each event carries an id: field — this takes precedence over
	// ?from_seq=, which exists for a first connection or manual/curl use.
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	if v := r.URL.Query().Get("from_seq"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}
