package queue

import (
	"context"
	"testing"
)

func TestQueue_RequestCancel_SetsFlagIdempotently(t *testing.T) {
	t.Parallel()
	job := seedJob(t)

	requested, err := IsCancelRequested(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("IsCancelRequested: %v", err)
	}
	if requested {
		t.Fatal("IsCancelRequested = true before any RequestCancel call")
	}

	if err := RequestCancel(context.Background(), testPool, job.ID); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	requested, err = IsCancelRequested(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("IsCancelRequested: %v", err)
	}
	if !requested {
		t.Fatal("IsCancelRequested = false after RequestCancel")
	}

	// A second call must not reset the deadline clock the sweeper's
	// wedged-worker sweep measures from — it's a no-op, not a re-stamp.
	first, err := getJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("getJob: %v", err)
	}
	if err := RequestCancel(context.Background(), testPool, job.ID); err != nil {
		t.Fatalf("RequestCancel (second call): %v", err)
	}
	second, err := getJob(context.Background(), testPool, job.ID)
	if err != nil {
		t.Fatalf("getJob: %v", err)
	}
	if !first.CancelRequestedAt.Equal(*second.CancelRequestedAt) {
		t.Errorf("cancel_requested_at changed on second call: %v -> %v", first.CancelRequestedAt, second.CancelRequestedAt)
	}
}
