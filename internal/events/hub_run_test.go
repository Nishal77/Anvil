package events

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeRedisSubscriber hands out one channel per Subscribe call and closes
// it as soon as ctx is cancelled — enough to exercise Hub's per-job
// dispatch goroutine lifecycle without a real Redis connection.
type fakeRedisSubscriber struct {
	ch chan []byte
}

func (f *fakeRedisSubscriber) Subscribe(ctx context.Context, _ string) (<-chan []byte, error) {
	go func() {
		<-ctx.Done()
	}()
	return f.ch, nil
}

func TestHub_Run_ExitsAfterCtxCancelled(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	redis := &fakeRedisSubscriber{ch: make(chan []byte)}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- h.Run(ctx, redis) }()

	// Subscribing opens this job's Redis subscription goroutine — the
	// thing Run must wait for on the way out.
	_, unsubscribe := h.Subscribe(uuid.New())
	defer unsubscribe()

	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return within 2s of ctx cancellation")
	}
}
