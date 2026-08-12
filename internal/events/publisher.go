package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/storage"
)

// store is the subset of storage.Store the publisher needs. Declared
// here, at the consumer, per CODE-STANDARDS §3.1.
type store interface {
	AppendEvent(ctx context.Context, jobID uuid.UUID, typ storage.EventType, payload json.RawMessage) (storage.Event, error)
}

// redisPublisher is the subset of a Redis client the publisher needs.
type redisPublisher interface {
	Publish(ctx context.Context, channel string, payload []byte) error
}

// Config configures a Publisher.
type Config struct {
	Store  store
	Redis  redisPublisher
	Logger *slog.Logger
}

func (c Config) validate() error {
	if c.Store == nil {
		return errors.New("events: config: Store is required")
	}
	if c.Redis == nil {
		return errors.New("events: config: Redis is required")
	}
	if c.Logger == nil {
		return errors.New("events: config: Logger is required")
	}
	return nil
}

// Publisher persists an event, then publishes it to Redis — always in
// that order. Persistence is the source of truth; the Redis publish is
// only there to wake up subscribers faster than they'd notice from
// polling. Losing the publish loses nothing durable.
type Publisher struct {
	store store
	redis redisPublisher
	log   *slog.Logger
}

// New constructs a Publisher from cfg, or returns an error if cfg is
// invalid.
func New(cfg Config) (*Publisher, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Publisher{store: cfg.Store, redis: cfg.Redis, log: cfg.Logger}, nil
}

// Publish persists typ/payload for jobID and, only once that write has
// committed, publishes it to the job's Redis channel so any open SSE
// connections hear about it right away. A Redis publish failure is
// logged, not returned: the event is already durable and readable
// through the REST fallback regardless of whether any browser tab is
// listening live — Redis being down is never a reason to fail a job.
func (p *Publisher) Publish(ctx context.Context, jobID uuid.UUID, typ storage.EventType, payload json.RawMessage) error {
	ev, err := p.store.AppendEvent(ctx, jobID, typ, payload)
	if err != nil {
		return fmt.Errorf("events: publish: %w", err)
	}

	raw, err := json.Marshal(ev)
	if err != nil {
		p.log.ErrorContext(ctx, "encode event for redis failed", slog.String("job_id", jobID.String()), slog.Any("err", err))
		return nil
	}
	if err := p.redis.Publish(ctx, channelName(jobID), raw); err != nil {
		p.log.WarnContext(ctx, "redis publish failed, event is still durable", slog.String("job_id", jobID.String()), slog.Any("err", err))
	}
	return nil
}
