package events

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/storage"
)

// fakeStore and fakeRedis let these tests control exactly what "the
// database" and "Redis" do, so the ordering guarantee under test —
// persist, then publish — can be checked directly instead of inferred
// from side effects.
type fakeStore struct {
	mu       sync.Mutex
	appended []storage.EventType
	nextSeq  int64
	appendFn func() error
}

func (f *fakeStore) AppendEvent(_ context.Context, jobID uuid.UUID, typ storage.EventType, payload json.RawMessage) (storage.Event, error) {
	if f.appendFn != nil {
		if err := f.appendFn(); err != nil {
			return storage.Event{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSeq++
	f.appended = append(f.appended, typ)
	return storage.Event{JobID: jobID, Seq: f.nextSeq, Type: typ, Payload: payload}, nil
}

type fakeRedis struct {
	mu        sync.Mutex
	published []string
	publishFn func() error
}

func (f *fakeRedis) Publish(_ context.Context, channel string, _ []byte) error {
	if f.publishFn != nil {
		if err := f.publishFn(); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, channel)
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestI7_EventPersistedBeforeRedisPublish(t *testing.T) {
	t.Parallel()

	var order []string
	var mu sync.Mutex
	store := &fakeStore{appendFn: func() error {
		mu.Lock()
		order = append(order, "persist")
		mu.Unlock()
		return nil
	}}
	redis := &fakeRedis{publishFn: func() error {
		mu.Lock()
		order = append(order, "publish")
		mu.Unlock()
		return nil
	}}

	pub, err := New(Config{Store: store, Redis: redis, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := pub.Publish(context.Background(), uuid.New(), "log_line", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if len(order) != 2 || order[0] != "persist" || order[1] != "publish" {
		t.Fatalf("call order = %v, want [persist publish]", order)
	}
}

func TestI8_RedisPublishFailureStillPersistsEvent(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	redis := &fakeRedis{publishFn: func() error { return errors.New("redis is down") }}

	pub, err := New(Config{Store: store, Redis: redis, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Redis being down must not be a reason to fail the publish — the
	// event is already durable in Postgres by the time Redis is even
	// touched (I-8: Redis is never a source of truth).
	if err := pub.Publish(context.Background(), uuid.New(), "log_line", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Publish() error = %v, want nil — a Redis failure must not fail the publish", err)
	}
	if len(store.appended) != 1 {
		t.Fatalf("store.appended = %v, want exactly one persisted event", store.appended)
	}
}

func TestPublisher_Publish_StoreFailurePropagatesAndSkipsRedis(t *testing.T) {
	t.Parallel()

	store := &fakeStore{appendFn: func() error { return errors.New("database is down") }}
	redis := &fakeRedis{}

	pub, err := New(Config{Store: store, Redis: redis, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := pub.Publish(context.Background(), uuid.New(), "log_line", json.RawMessage(`{}`)); err == nil {
		t.Fatal("Publish() error = nil, want an error when the store fails")
	}
	if len(redis.published) != 0 {
		t.Fatalf("redis.published = %v, want none — a failed persist must never reach Redis", redis.published)
	}
}
