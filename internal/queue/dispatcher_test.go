package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTestDispatcher(t *testing.T, runStep func(ctx context.Context, job *Job) error) *Dispatcher {
	t.Helper()
	d, err := New(Config{
		Pool:              testPool,
		Logger:            testLogger(),
		WorkerID:          t.Name(),
		NumWorkers:        1,
		LeaseTTL:          2 * time.Second,
		HeartbeatInterval: 400 * time.Millisecond,
		SweepInterval:     200 * time.Millisecond,
		RunStep:           runStep,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return d
}

func TestDispatcher_Run_ClaimsExecutesAndSucceeds(t *testing.T) {
	job := seedQueuedJob(t)
	d := newTestDispatcher(t, func(context.Context, *Job) error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitFor(t, 5*time.Second, func() bool {
		row, err := getJob(context.Background(), testPool, job.ID)
		return err == nil && row.Status == StatusSucceeded
	}, func() {
		t.Fatal("job never reached SUCCEEDED")
	})
	cancel()
	<-done
}

func TestDispatcher_Run_FailedStepTransitionsToFailedWithReason(t *testing.T) {
	job := seedQueuedJob(t)
	wantErr := errors.New("step exploded")
	d := newTestDispatcher(t, func(context.Context, *Job) error { return wantErr })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	var row *Job
	waitFor(t, 5*time.Second, func() bool {
		var err error
		row, err = getJob(context.Background(), testPool, job.ID)
		return err == nil && row.Status == StatusFailed
	}, func() {
		t.Fatal("job never reached FAILED")
	})
	cancel()
	<-done

	if row.FailureReason != wantErr.Error() {
		t.Errorf("FailureReason = %q, want %q", row.FailureReason, wantErr.Error())
	}
}

func TestDispatcher_Run_GracefulShutdownReclaimsInFlightJob(t *testing.T) {
	job := seedQueuedJob(t)
	stepStarted := make(chan struct{})
	blockUntilCancelled := make(chan struct{})
	d := newTestDispatcher(t, func(ctx context.Context, _ *Job) error {
		close(stepStarted)
		<-ctx.Done() // honors cancellation, like a well-behaved step must
		close(blockUntilCancelled)
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	select {
	case <-stepStarted:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("step never started")
	}

	cancel() // simulate SIGTERM: Run's ctx is cancelled mid-job

	select {
	case <-blockUntilCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("RunStep never observed cancellation")
	}
	<-done

	// Graceful shutdown must reclaim, not strand: status back to
	// claimable, lease cleared, run_after immediate (not backed off).
	waitFor(t, 2*time.Second, func() bool {
		row, err := getJob(context.Background(), testPool, job.ID)
		return err == nil && row.Status == StatusQueued && row.LeaseOwner == ""
	}, func() {
		row, _ := getJob(context.Background(), testPool, job.ID)
		t.Fatalf("job was not reclaimed on shutdown, last state: %+v", row)
	})
}

func TestQueue_New_RejectsHeartbeatIntervalNotLessThanHalfLeaseTTL(t *testing.T) {
	_, err := New(Config{
		Pool:              testPool,
		Logger:            testLogger(),
		RunStep:           func(context.Context, *Job) error { return nil },
		LeaseTTL:          10 * time.Second,
		HeartbeatInterval: 6 * time.Second, // >= LeaseTTL/2
	})
	if err == nil {
		t.Fatal("New() succeeded with HeartbeatInterval >= LeaseTTL/2, want a startup error")
	}
}

func TestQueue_New_RequiresRunStep(t *testing.T) {
	_, err := New(Config{Pool: testPool, Logger: testLogger()})
	if err == nil {
		t.Fatal("New() succeeded with no RunStep, want a startup error")
	}
}
