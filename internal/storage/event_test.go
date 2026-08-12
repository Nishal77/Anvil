package storage

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// seedJobForEvents inserts a user and a job so job_events' foreign key has
// something to reference, and returns the job's ID.
func seedJobForEvents(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	user, err := testStore.CreateUser(ctx, uniqueEmail(t), "hash")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	var jobID uuid.UUID
	err = testStore.pool.QueryRow(ctx,
		`INSERT INTO jobs (user_id, prompt) VALUES ($1, $2) RETURNING id`,
		user.ID, "test prompt",
	).Scan(&jobID)
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return jobID
}

func TestFR050_EventSeqIsMonotonicPerJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	jobID := seedJobForEvents(t)

	const callers = 8
	const perCaller = 10

	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			for range perCaller {
				if _, err := testStore.AppendEvent(ctx, jobID, "log_line", json.RawMessage(`{}`)); err != nil {
					t.Errorf("AppendEvent: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	events, err := testStore.ListEventsFrom(ctx, jobID, 0)
	if err != nil {
		t.Fatalf("ListEventsFrom: %v", err)
	}
	if len(events) != callers*perCaller {
		t.Fatalf("got %d events, want %d", len(events), callers*perCaller)
	}

	seen := make(map[int64]bool, len(events))
	for i, ev := range events {
		wantSeq := int64(i + 1)
		if ev.Seq != wantSeq {
			t.Errorf("events[%d].Seq = %d, want %d (a gap or duplicate)", i, ev.Seq, wantSeq)
		}
		if seen[ev.Seq] {
			t.Errorf("duplicate seq %d", ev.Seq)
		}
		seen[ev.Seq] = true
	}
}

func TestAppendEvent_ReturnsWhatItPersisted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	jobID := seedJobForEvents(t)

	payload := json.RawMessage(`{"hello":"world"}`)
	ev, err := testStore.AppendEvent(ctx, jobID, "log_line", payload)
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if ev.JobID != jobID {
		t.Errorf("JobID = %v, want %v", ev.JobID, jobID)
	}
	if ev.Type != "log_line" {
		t.Errorf("Type = %q, want %q", ev.Type, "log_line")
	}
	if string(ev.Payload) != string(payload) {
		t.Errorf("Payload = %s, want %s", ev.Payload, payload)
	}
}

func TestListEventsFrom_ReturnsOnlyEventsAfterFromSeq(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	jobID := seedJobForEvents(t)

	var last Event
	for range 5 {
		ev, err := testStore.AppendEvent(ctx, jobID, "log_line", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		last = ev
	}

	events, err := testStore.ListEventsFrom(ctx, jobID, last.Seq-1)
	if err != nil {
		t.Fatalf("ListEventsFrom: %v", err)
	}
	if len(events) != 1 || events[0].Seq != last.Seq {
		t.Fatalf("ListEventsFrom(lastSeq-1) = %v, want exactly the last event", events)
	}

	events, err = testStore.ListEventsFrom(ctx, jobID, last.Seq)
	if err != nil {
		t.Fatalf("ListEventsFrom: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("ListEventsFrom(lastSeq) = %v, want none", events)
	}
}

func TestListEventsFrom_SeparateJobsDoNotInterfere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	jobA := seedJobForEvents(t)
	jobB := seedJobForEvents(t)

	if _, err := testStore.AppendEvent(ctx, jobA, "log_line", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("AppendEvent(jobA): %v", err)
	}

	eventsB, err := testStore.ListEventsFrom(ctx, jobB, 0)
	if err != nil {
		t.Fatalf("ListEventsFrom(jobB): %v", err)
	}
	if len(eventsB) != 0 {
		t.Fatalf("jobB has %d events, want 0 — jobA's event leaked across jobs", len(eventsB))
	}
}
