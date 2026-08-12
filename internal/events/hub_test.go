package events

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/storage"
)

// These tests exercise Hub.broadcast and subscriber.send directly rather
// than going through a real (or fake) Redis subscription — Hub's job of
// fanning out to subscribers without blocking or silently dropping is
// independent of where the events came from, and testing it this way
// keeps these tests fast and deterministic.

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	h, err := NewHub(HubConfig{Logger: discardLogger()})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	return h
}

func TestFR054_SubscriberBufferBounded1024(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	jobID := uuid.New()

	ch, unsubscribe := h.Subscribe(jobID)
	defer unsubscribe()

	// Send far more than the buffer can hold, without ever reading from
	// ch — send must never block on a full buffer.
	for i := range subscriberBufferSize * 2 {
		h.broadcast(jobID, storage.Event{Seq: int64(i + 1), Type: "log_line"})
	}

	if n := len(ch); n > subscriberBufferSize {
		t.Fatalf("channel holds %d buffered events, want at most %d", n, subscriberBufferSize)
	}
}

func TestFR054_SlowSubscriberGetsStreamGapNotSilentDrop(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	jobID := uuid.New()

	ch, unsubscribe := h.Subscribe(jobID)
	defer unsubscribe()

	// Overflow the buffer well past its capacity, still without reading —
	// the channel is now completely full, so there's no room for
	// anything, gap marker included.
	const sent = subscriberBufferSize + 50
	for i := range sent {
		h.broadcast(jobID, storage.Event{Seq: int64(i + 1), Type: "log_line"})
	}

	// A real reader drains continuously, so drops only ever happen in the
	// brief window before it catches back up. Simulate that: read a
	// handful of events to free some room, then publish one more — that
	// gives flushGap (see subscriber.go) an actual slot to use.
	for range 10 {
		<-ch
	}
	h.broadcast(jobID, storage.Event{Seq: sent + 1, Type: "job_finished"})

	sawGap, gapCount, lastRealSeq := drainAndCountGaps(t, ch)
	if !sawGap {
		t.Fatal("subscriber never received a stream_gap event after its buffer overflowed")
	}
	if gapCount != 1 {
		t.Errorf("received %d stream_gap events, want exactly 1", gapCount)
	}
	if lastRealSeq == 0 {
		t.Error("subscriber received no real events at all, only the gap marker")
	}
}

// drainAndCountGaps reads every currently-buffered event off ch,
// validating each stream_gap payload it finds, and reports whether it
// saw one, how many, and the highest real (non-gap) seq observed.
func drainAndCountGaps(t *testing.T, ch <-chan storage.Event) (sawGap bool, gapCount int, lastRealSeq int64) {
	t.Helper()
	for len(ch) > 0 {
		ev := <-ch
		if ev.Type == eventTypeStreamGap {
			sawGap = true
			gapCount++
			var payload struct {
				FromSeq int64 `json:"from_seq"`
				ToSeq   int64 `json:"to_seq"`
			}
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatalf("unmarshal stream_gap payload: %v", err)
			}
			if payload.FromSeq == 0 || payload.ToSeq == 0 || payload.FromSeq > payload.ToSeq {
				t.Errorf("stream_gap payload = %+v, want a valid non-empty range", payload)
			}
			continue
		}
		lastRealSeq = ev.Seq
	}
	return sawGap, gapCount, lastRealSeq
}

func TestHub_Subscribe_UnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	jobID := uuid.New()

	ch, unsubscribe := h.Subscribe(jobID)
	unsubscribe()

	h.broadcast(jobID, storage.Event{Seq: 1, Type: "log_line"})

	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("received %+v after unsubscribe, want no delivery", ev)
		}
	case <-time.After(100 * time.Millisecond):
		// No delivery within a reasonable window — the channel is simply
		// no longer receiving broadcasts, which is correct: unsubscribe
		// doesn't close the channel, it just removes it from future
		// broadcasts.
	}
}

func TestHub_Broadcast_OnlyReachesThatJobsSubscribers(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	jobA, jobB := uuid.New(), uuid.New()

	chA, unsubA := h.Subscribe(jobA)
	defer unsubA()
	chB, unsubB := h.Subscribe(jobB)
	defer unsubB()

	h.broadcast(jobA, storage.Event{Seq: 1, Type: "log_line"})

	select {
	case <-chA:
	default:
		t.Fatal("jobA's subscriber received nothing")
	}
	select {
	case ev := <-chB:
		t.Fatalf("jobB's subscriber received %+v, want nothing — broadcast leaked across jobs", ev)
	default:
	}
}
