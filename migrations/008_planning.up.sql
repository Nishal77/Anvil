-- Week 7 additions: the planner's output, auto-approve, and
-- cancellation (PRD §12.1, §13.3). Additive only — no existing column
-- changes type or drops.

ALTER TABLE jobs ADD COLUMN plan_summary TEXT;
ALTER TABLE jobs ADD COLUMN plan_risks JSONB;

-- PRD §11's POST /v1/jobs request body already documents
-- options.auto_approve; jobs had no column for it until now.
ALTER TABLE jobs ADD COLUMN auto_approve BOOLEAN NOT NULL DEFAULT false;

-- §13.3 step 1: POST /cancel sets this. NULL means no cancellation
-- requested. Indexed for the sweeper's wedged-worker sweep (step 5),
-- which scans for jobs with a lease-holding status AND a cancellation
-- request older than the acknowledgement deadline.
ALTER TABLE jobs ADD COLUMN cancel_requested_at TIMESTAMPTZ;
CREATE INDEX idx_jobs_cancel_requested ON jobs (cancel_requested_at)
    WHERE cancel_requested_at IS NOT NULL;

-- PlannedStep.Acceptance / Optional (PRD §12.1) — how the executor
-- knows a step succeeded, and whether repair-exhaustion should SKIP
-- rather than fail the job (PRD §12.4).
ALTER TABLE steps ADD COLUMN acceptance TEXT NOT NULL DEFAULT '';
ALTER TABLE steps ADD COLUMN optional BOOLEAN NOT NULL DEFAULT false;
