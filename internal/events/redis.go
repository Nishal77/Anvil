package events

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// redisSubscribeBufferSize bounds the translation channel between a raw
// Redis subscription and dispatchLoop. Redis pub/sub already has its own
// backpressure story; this just needs enough room that a brief scheduling
// delay in dispatchLoop doesn't cause a needless block.
const redisSubscribeBufferSize = 64

// RedisClient adapts go-redis to the narrow redisPublisher and
// redisSubscriber interfaces this package actually needs, so nothing else
// in the codebase has to import go-redis directly.
type RedisClient struct {
	client *redis.Client
}

// NewRedisClient constructs a RedisClient connected to addr (host:port).
// It doesn't dial eagerly — the connection is established lazily on first
// use, matching go-redis's own behavior.
func NewRedisClient(addr string) *RedisClient {
	return &RedisClient{client: redis.NewClient(&redis.Options{Addr: addr})}
}

// Close releases the underlying connection pool.
func (r *RedisClient) Close() error {
	if err := r.client.Close(); err != nil {
		return fmt.Errorf("events: close redis client: %w", err)
	}
	return nil
}

// Publish sends payload on channel.
func (r *RedisClient) Publish(ctx context.Context, channel string, payload []byte) error {
	if err := r.client.Publish(ctx, channel, payload).Err(); err != nil {
		return fmt.Errorf("events: redis publish to %s: %w", channel, err)
	}
	return nil
}

// Subscribe returns a channel of raw message payloads on channel. The
// returned channel closes once ctx is cancelled or the underlying Redis
// subscription itself closes — whichever happens first.
func (r *RedisClient) Subscribe(ctx context.Context, channel string) (<-chan []byte, error) {
	pubsub := r.client.Subscribe(ctx, channel)
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, fmt.Errorf("events: redis subscribe to %s: %w", channel, err)
	}

	out := make(chan []byte, redisSubscribeBufferSize)

	// Owned by this call: exits the instant ctx is cancelled. Its only
	// job is closing pubsub, which is what unblocks the range loop below
	// — that loop has no other way to notice ctx was cancelled while it's
	// parked waiting on pubsub.Channel().
	go func() {
		<-ctx.Done()
		_ = pubsub.Close()
	}()

	// Owned by this call: exits when pubsub.Channel() closes (triggered
	// by the goroutine above, or by the connection dropping) or when ctx
	// is cancelled directly.
	go func() {
		defer close(out)
		for msg := range pubsub.Channel() {
			select {
			case out <- []byte(msg.Payload):
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}
