// Package queue owns the transition of a job from persisted to worked-on,
// exactly once, durably (PRD §9.2, §14). A job is claimable when its status
// is PENDING_PLAN or QUEUED and its run_after has elapsed. Workers claim
// jobs with FOR UPDATE SKIP LOCKED, which serializes claims across an
// arbitrary number of workers without any external coordination service.
//
// A claim grants a time-bounded lease. The owning worker extends it by
// heartbeat; if the worker dies, the lease expires and a sweeper makes the
// job claimable again with its attempt counter already incremented. This is
// what makes the system durable across control-plane crashes: see
// docs/PRD.md §14 for the full model and §14.3 for the recovery matrix.
//
// Every status write goes through Transition — the single guarded
// function referenced by CLAUDE.md invariant I-1. No other code in this
// repository issues `UPDATE jobs SET status`.
//
// Entry points:
//
//	New        — construct a Dispatcher from Config
//	Run        — start workers and sweeper; blocks until ctx is cancelled
//	Transition — the guarded state-change function (invariant I-1)
package queue
