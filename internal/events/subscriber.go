package events

import (
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/anvil-dev/anvil/internal/storage"
)

// subscriberBufferSize bounds how many events one subscriber can be
// behind before it starts missing them. 1024 events at typical log-line
// size is a small, fixed amount of memory per open browser tab.
const subscriberBufferSize = 1024

const eventTypeStreamGap storage.EventType = "stream_gap"

// subscriber is one open SSE connection's view of a job's event stream.
type subscriber struct {
	ch chan storage.Event // buffered subscriberBufferSize; never blocks a publish

	// dropFrom, dropTo, and dropped track a run of events this subscriber
	// missed because its buffer was full. Once there's room again, send
	// delivers one stream_gap event describing that whole range before
	// resuming normal delivery — the client can then re-fetch exactly
	// what it missed instead of just silently having a hole in its log.
	dropFrom atomic.Int64
	dropTo   atomic.Int64
	dropped  atomic.Int64
}

func newSubscriber() *subscriber {
	return &subscriber{ch: make(chan storage.Event, subscriberBufferSize)}
}

// send delivers ev to this subscriber without ever blocking the caller.
// If the buffer is full, it records the drop instead of waiting — one
// stalled browser tab must never hold up delivery to every other
// subscriber of this job.
func (s *subscriber) send(ev storage.Event) {
	if s.dropped.Load() > 0 {
		s.flushGap()
	}

	select {
	case s.ch <- ev:
	default:
		s.dropFrom.CompareAndSwap(0, ev.Seq)
		s.dropTo.Store(ev.Seq)
		s.dropped.Add(1)
	}
}

// flushGap tries to deliver a stream_gap event summarizing whatever this
// subscriber just missed. It's best-effort and non-blocking like send:
// if the buffer is still full, it just tries again on the next send.
// In practice that's fine — a real reader (the SSE writer) drains
// continuously, so the buffer being full is only ever momentary, and the
// next event to arrive finds room.
//
// ponytail: this only gets retried when another event arrives for this
// job, so a subscriber whose buffer overflows on the very last event a
// job will ever publish could in theory miss the gap marker too. In
// practice every job's stream ends with at least one more event
// (step_finished, job_finished, ...), so this hasn't been a real problem
// — if it ever needs to be airtight regardless of what a job does next,
// have the SSE writer's heartbeat tick call flushGap directly instead of
// only send doing it.
func (s *subscriber) flushGap() {
	from, to := s.dropFrom.Load(), s.dropTo.Load()
	payload, err := json.Marshal(struct {
		FromSeq int64 `json:"from_seq"`
		ToSeq   int64 `json:"to_seq"`
	}{FromSeq: from, ToSeq: to})
	if err != nil {
		return
	}

	gap := storage.Event{Seq: to, Type: eventTypeStreamGap, Payload: payload, CreatedAt: time.Now()}
	select {
	case s.ch <- gap:
		s.dropFrom.Store(0)
		s.dropTo.Store(0)
		s.dropped.Store(0)
	default:
	}
}
