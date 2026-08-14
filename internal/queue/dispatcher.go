package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

const pollInterval = 2 * time.Second

// Config configures a Dispatcher.
type Config struct {
	Pool              *pgxpool.Pool
	Logger            *slog.Logger
	Clock             Clock // nil defaults to realClock{}
	WorkerID          string
	NumWorkers        int           // default 4, PRD §9.2
	LeaseTTL          time.Duration // default 60s
	HeartbeatInterval time.Duration // default 15s; must be < LeaseTTL/2
	SweepInterval     time.Duration // default 30s

	// RunStep executes one claimed job's work. A plain func, not an
	// interface — Week 4 decides what actually drives sandbox+events;
	// this week only needs something a test can control deterministically.
	RunStep func(ctx context.Context, job *Job) error

	// DestroySandbox force-destroys a sandbox by ID, by container/sandbox
	// identifier alone — no job to hand off to, since the worker that
	// owned it never acknowledged the cancel (PRD §13.3 step 5). Optional:
	// nil is a no-op, so tests that don't exercise the wedged-worker path
	// don't need to supply one.
	DestroySandbox func(ctx context.Context, sandboxID string) error
}

func (c *Config) setDefaults() {
	if c.Clock == nil {
		c.Clock = realClock{}
	}
	if c.NumWorkers <= 0 {
		c.NumWorkers = 4
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 60 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 15 * time.Second
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = 30 * time.Second
	}
	if c.WorkerID == "" {
		c.WorkerID = "worker"
	}
}

func (c Config) validate() error {
	if c.Pool == nil {
		return errors.New("queue: config: Pool is required")
	}
	if c.Logger == nil {
		return errors.New("queue: config: Logger is required")
	}
	if c.RunStep == nil {
		return errors.New("queue: config: RunStep is required")
	}
	// Two heartbeat attempts must fit inside one lease, or a single slow
	// heartbeat can let the lease expire out from under a live worker.
	// A startup error, not a production incident.
	if c.HeartbeatInterval >= c.LeaseTTL/2 {
		return fmt.Errorf("queue: config: HeartbeatInterval (%s) must be less than half LeaseTTL (%s)", c.HeartbeatInterval, c.LeaseTTL)
	}
	return nil
}

// Dispatcher owns the worker pool and the lease-reclaim sweeper.
type Dispatcher struct {
	pool              *pgxpool.Pool
	log               *slog.Logger
	clock             Clock
	workerIDPrefix    string
	numWorkers        int
	leaseTTL          time.Duration
	heartbeatInterval time.Duration
	sweepInterval     time.Duration
	runStep           func(ctx context.Context, job *Job) error
	destroySandbox    func(ctx context.Context, sandboxID string) error
}

// New constructs a Dispatcher from cfg, or returns an error if cfg is
// invalid.
func New(cfg Config) (*Dispatcher, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Dispatcher{
		pool:              cfg.Pool,
		log:               cfg.Logger,
		clock:             cfg.Clock,
		workerIDPrefix:    cfg.WorkerID,
		numWorkers:        cfg.NumWorkers,
		leaseTTL:          cfg.LeaseTTL,
		heartbeatInterval: cfg.HeartbeatInterval,
		sweepInterval:     cfg.SweepInterval,
		runStep:           cfg.RunStep,
		destroySandbox:    cfg.DestroySandbox,
	}, nil
}

// Run starts NumWorkers worker goroutines and one sweeper goroutine, and
// blocks until ctx is cancelled. Every started goroutine has exited before
// Run returns (CLAUDE.md I-5, CODE-STANDARDS C1-C3): workers release any
// held lease before exiting; the sweeper exits at its next tick check.
func (d *Dispatcher) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)

	for i := range d.numWorkers {
		workerID := fmt.Sprintf("%s-%d", d.workerIDPrefix, i)
		g.Go(func() error { return d.worker(gctx, workerID) })
	}
	g.Go(func() error { return d.sweepLoop(gctx) })

	if err := g.Wait(); err != nil {
		return fmt.Errorf("queue: dispatcher run: %w", err)
	}
	return nil
}

// worker claims jobs in a loop until ctx is cancelled. Transient claim
// errors are logged and retried, not treated as fatal — one bad poll must
// not take down the whole dispatcher.
func (d *Dispatcher) worker(ctx context.Context, workerID string) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		job, err := Claim(ctx, d.pool, workerID, d.leaseTTL)
		if err != nil {
			if !errors.Is(err, ErrNoWork) {
				d.log.ErrorContext(ctx, "claim failed", slog.String("worker_id", workerID), slog.Any("err", err))
			}
			select {
			case <-ctx.Done():
				return nil
			case <-d.clock.After(pollInterval):
			}
			continue
		}

		d.runJob(ctx, workerID, job)
	}
}

// runJob heartbeats job's lease for the duration of runStep, then
// transitions it to a terminal state — or, if ctx was cancelled mid-job,
// reclaims it immediately instead of stranding it for the full lease TTL
// (graceful shutdown must not abandon leases).
func (d *Dispatcher) runJob(ctx context.Context, workerID string, job *Job) {
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		d.heartbeatLoop(hbCtx, workerID, job)
	}()

	stepErr := d.runStep(ctx, job)

	stopHeartbeat()
	<-hbDone // no heartbeat goroutine outlives runJob (CLAUDE.md I-5)

	if ctx.Err() != nil {
		d.reclaimOnShutdown(ctx, job)
		return
	}

	if stepErr != nil {
		reason := stepErr.Error()
		if err := Transition(ctx, d.pool, job.ID, job.Status, StatusFailed, JobStatusFields{FailureReason: &reason}); err != nil {
			d.log.ErrorContext(ctx, "transition to FAILED failed", slog.Any("err", err))
		}
		return
	}

	// runStep may already have moved the job on itself: the planner
	// lands a claimed PLANNING job in AWAITING_APPROVAL or QUEUED, not
	// SUCCEEDED, and a cancelled job lands in CANCELLED. Only
	// auto-succeed a job still sitting in the status it was claimed
	// into — anything else means runStep already wrote the real outcome.
	current, err := getJob(ctx, d.pool, job.ID)
	if err != nil {
		d.log.ErrorContext(ctx, "reload job after run failed", slog.Any("err", err))
		return
	}
	if current.Status != job.Status {
		return
	}
	if err := Transition(ctx, d.pool, job.ID, job.Status, StatusSucceeded, JobStatusFields{}); err != nil {
		d.log.ErrorContext(ctx, "transition to SUCCEEDED failed", slog.Any("err", err))
	}
}

// reclaimOnShutdown accepts the already-cancelled ctx for logging only —
// the cleanup write itself is deliberately rooted at context.Background(),
// not derived from ctx, since ctx is Done by the time this runs and a
// child of it would expire immediately.
func (d *Dispatcher) reclaimOnShutdown(ctx context.Context, job *Job) {
	target, ok := reclaimTarget[job.Status]
	if !ok {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reclaimJobAt(shutdownCtx, d.pool, job.ID, job.Status, target, d.clock.Now()); err != nil { //nolint:contextcheck // reason: shutdownCtx is intentionally rooted at context.Background(), not ctx — see the doc comment above
		d.log.ErrorContext(ctx, "reclaim on shutdown failed", slog.Any("err", err))
	}
}

func (d *Dispatcher) heartbeatLoop(ctx context.Context, workerID string, job *Job) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.clock.After(d.heartbeatInterval):
			if err := Heartbeat(ctx, d.pool, job.ID, workerID, d.leaseTTL); err != nil {
				if !errors.Is(err, ErrLeaseLost) {
					d.log.ErrorContext(ctx, "heartbeat failed", slog.Any("err", err))
				}
				return
			}
		}
	}
}

func (d *Dispatcher) sweepLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.clock.After(d.sweepInterval):
			reclaimed, deadLettered, cancelled, cancelledSandboxIDs, err := sweep(ctx, d.pool, d.clock)
			if err != nil {
				d.log.ErrorContext(ctx, "sweep failed", slog.Any("err", err))
				continue
			}
			if reclaimed > 0 || deadLettered > 0 || cancelled > 0 {
				d.log.InfoContext(ctx, "sweep completed",
					slog.Int("reclaimed", reclaimed), slog.Int("dead_lettered", deadLettered), slog.Int("cancelled", cancelled))
			}
			d.destroyWedgedSandboxes(ctx, cancelledSandboxIDs)
		}
	}
}

// destroyWedgedSandboxes force-destroys every sandbox a wedged,
// force-cancelled job left running (PRD §13.3 step 5). Best-effort: a
// destroy failure is logged, not fatal to the sweep loop — the sandbox
// will still be caught by the runner's own resource limits, and the job
// itself has already reached CANCELLED regardless.
func (d *Dispatcher) destroyWedgedSandboxes(ctx context.Context, sandboxIDs []string) {
	if d.destroySandbox == nil {
		return
	}
	for _, id := range sandboxIDs {
		if err := d.destroySandbox(ctx, id); err != nil {
			d.log.ErrorContext(ctx, "force-destroy wedged sandbox failed", slog.String("sandbox_id", id), slog.Any("err", err))
		}
	}
}
