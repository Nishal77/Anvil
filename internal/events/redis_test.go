package events

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startTestRedis boots a real Redis container — CLAUDE.md T5, and the
// only way to exercise RedisClient's actual go-redis wiring rather than
// the fakeRedisSubscriber every other test in this package uses.
// Generic container request, not a dedicated testcontainers module: no
// redis-specific module is a project dependency, and one container
// image is not worth adding one for.
func startTestRedis(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	ctx := context.Background()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("get redis host: %v", err)
	}
	port, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("get redis port: %v", err)
	}
	return host + ":" + port.Port()
}

// TestRedisClient_PublishSubscribeRoundTrip proves RedisClient's real
// go-redis wiring — the fakes every other test in this package uses
// don't exercise NewRedisClient, Publish, Subscribe, or Close at all.
func TestRedisClient_PublishSubscribeRoundTrip(t *testing.T) {
	addr := startTestRedis(t)
	client := NewRedisClient(addr)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := client.Subscribe(ctx, "test-channel")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := client.Publish(context.Background(), "test-channel", []byte("hello")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case msg := <-ch:
		if string(msg) != "hello" {
			t.Errorf("received %q, want %q", msg, "hello")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive published message within 5s")
	}
}

// TestRedisClient_SubscribeChannelClosesOnCtxCancel proves the
// subscription's output channel actually closes once ctx is
// cancelled — the behavior Hub's dispatch loop depends on to notice a
// subscription ending.
func TestRedisClient_SubscribeChannelClosesOnCtxCancel(t *testing.T) {
	addr := startTestRedis(t)
	client := NewRedisClient(addr)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := client.Subscribe(ctx, "test-channel-2")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel produced a value instead of closing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel did not close within 5s of ctx cancellation")
	}
}
