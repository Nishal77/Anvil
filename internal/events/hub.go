package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/storage"
)

// redisSubscriber is the subset of a Redis client the hub needs.
type redisSubscriber interface {
	Subscribe(ctx context.Context, channel string) (<-chan []byte, error)
}

// HubConfig configures a Hub.
type HubConfig struct {
	Logger *slog.Logger
}

// Hub fans out published events to every open SSE connection subscribed
// to a job. It turns Redis's one channel per job into per-subscriber
// bounded channels — see subscriber.go for how a slow subscriber is
// handled without blocking anyone else.
type Hub struct {
	log *slog.Logger

	// mu guards subs. Held only for map access, never across a channel
	// send — holding it during a send would let one slow subscriber
	// block every publish in the process.
	mu   sync.RWMutex
	subs map[uuid.UUID][]*subscriber

	// redisMu guards redis, runCtx, and jobChannels — the state Subscribe
	// needs to lazily open a Redis subscription for a job the first time
	// someone actually asks for its events. wg tracks the goroutine each
	// of those subscriptions owns, so Run doesn't return until every one
	// has exited (CLAUDE.md I-5).
	//
	// ponytail: a per-job Redis subscription is kept open for the Hub's
	// whole lifetime once opened, even after every subscriber leaves —
	// fine at the handful of concurrent jobs Phase 1 targets. Close it on
	// zero-subscribers if job cardinality ever makes that idle cost matter.
	redisMu     sync.Mutex
	redis       redisSubscriber
	runCtx      context.Context
	jobChannels map[uuid.UUID]bool
	wg          sync.WaitGroup
}

// NewHub constructs a Hub from cfg, or returns an error if cfg is
// invalid.
func NewHub(cfg HubConfig) (*Hub, error) {
	if cfg.Logger == nil {
		return nil, errors.New("events: hub config: Logger is required")
	}
	return &Hub{
		log:         cfg.Logger,
		subs:        make(map[uuid.UUID][]*subscriber),
		jobChannels: make(map[uuid.UUID]bool),
	}, nil
}

// Run makes redis available to Subscribe and blocks until ctx is
// cancelled, then waits for every per-job subscription goroutine it
// started to exit before returning — none of them outlives Run.
func (h *Hub) Run(ctx context.Context, redis redisSubscriber) error {
	h.redisMu.Lock()
	h.redis = redis
	h.runCtx = ctx
	h.redisMu.Unlock()

	<-ctx.Done()
	h.wg.Wait()
	return nil
}

// Subscribe registers a new subscriber for jobID with a bounded buffer
// and returns a receive-only channel of its events, plus a func to
// unregister it. The first Subscribe call for a given job opens that
// job's Redis subscription; later ones reuse it.
func (h *Hub) Subscribe(jobID uuid.UUID) (<-chan storage.Event, func()) {
	sub := newSubscriber()

	h.mu.Lock()
	h.subs[jobID] = append(h.subs[jobID], sub)
	h.mu.Unlock()

	h.ensureRedisSubscription(jobID)
	sseSubscribers.Inc()

	unsubscribe := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		subs := h.subs[jobID]
		for i, s := range subs {
			if s == sub {
				h.subs[jobID] = append(subs[:i], subs[i+1:]...)
				sseSubscribers.Dec() // only on an actual removal — unsubscribe may be called more than once
				break
			}
		}
		if len(h.subs[jobID]) == 0 {
			delete(h.subs, jobID)
		}
	}
	return sub.ch, unsubscribe
}

func (h *Hub) ensureRedisSubscription(jobID uuid.UUID) {
	h.redisMu.Lock()
	defer h.redisMu.Unlock()

	if h.redis == nil || h.jobChannels[jobID] {
		return
	}
	h.jobChannels[jobID] = true

	ch, err := h.redis.Subscribe(h.runCtx, channelName(jobID))
	if err != nil {
		h.log.ErrorContext(h.runCtx, "subscribe to job channel failed", slog.String("job_id", jobID.String()), slog.Any("err", err))
		delete(h.jobChannels, jobID)
		return
	}

	h.wg.Add(1)
	// Owned by Hub.Run: exits when ctx is cancelled (Run's caller) or when
	// the Redis subscription itself closes, whichever comes first — see
	// the loop below.
	go func() {
		defer h.wg.Done()
		h.dispatchLoop(h.runCtx, jobID, ch)
	}()
}

func (h *Hub) dispatchLoop(ctx context.Context, jobID uuid.UUID, ch <-chan []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-ch:
			if !ok {
				return
			}
			var ev storage.Event
			if err := json.Unmarshal(raw, &ev); err != nil {
				h.log.ErrorContext(ctx, "decode event from redis failed", slog.Any("err", err))
				continue
			}
			h.broadcast(jobID, ev)
		}
	}
}

func (h *Hub) broadcast(jobID uuid.UUID, ev storage.Event) {
	h.mu.RLock()
	subs := append([]*subscriber(nil), h.subs[jobID]...)
	h.mu.RUnlock()

	sseEventsPublishedTotal.WithLabelValues(string(ev.Type)).Inc()
	sseDeliveryLatency.Observe(time.Since(ev.CreatedAt).Seconds())
	for _, s := range subs {
		s.send(ev)
	}
}

func channelName(jobID uuid.UUID) string {
	return fmt.Sprintf("job:%s:events", jobID)
}
