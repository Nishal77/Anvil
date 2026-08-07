package api

import (
	"context"
	"testing"
	"time"
)

func TestServer_Run_ShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeAuth{}, &fakePinger{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() returned error after shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return within the shutdown grace period")
	}
}
