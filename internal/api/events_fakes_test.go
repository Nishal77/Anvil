package api

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/storage"
)

// fakeHub is a minimal eventHub — CODE-STANDARDS §3.1: the fake has
// exactly the methods the interface needs, not a mock framework.
type fakeHub struct {
	mu   sync.Mutex
	subs map[uuid.UUID][]chan storage.Event
}

func (f *fakeHub) Subscribe(jobID uuid.UUID) (<-chan storage.Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = make(map[uuid.UUID][]chan storage.Event)
	}
	ch := make(chan storage.Event, 1024)
	f.subs[jobID] = append(f.subs[jobID], ch)
	return ch, func() {}
}

// fakeEventStore is a minimal eventStore.
type fakeEventStore struct {
	mu     sync.Mutex
	events map[uuid.UUID][]storage.Event
}

func (f *fakeEventStore) ListEventsFrom(_ context.Context, jobID uuid.UUID, fromSeq int64) ([]storage.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []storage.Event
	for _, ev := range f.events[jobID] {
		if ev.Seq > fromSeq {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (f *fakeEventStore) seed(jobID uuid.UUID, events ...storage.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.events == nil {
		f.events = make(map[uuid.UUID][]storage.Event)
	}
	f.events[jobID] = append(f.events[jobID], events...)
}

// fakePublisher is a minimal eventPublisher.
type fakePublisher struct {
	mu     sync.Mutex
	events []storage.EventType
}

func (f *fakePublisher) Publish(_ context.Context, _ uuid.UUID, typ storage.EventType, _ json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, typ)
	return nil
}
