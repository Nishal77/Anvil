CREATE TYPE job_status AS ENUM (
    'PENDING_PLAN',       -- accepted, not yet planned
    'PLANNING',           -- planner LLM call in flight
    'AWAITING_APPROVAL',  -- plan ready, waiting on user
    'QUEUED',             -- approved, waiting for a worker
    'RUNNING',            -- executing steps
    'DEPLOYING',          -- steps done, building preview
    'SUCCEEDED',
    'FAILED',
    'CANCELLED'
);

CREATE TABLE jobs (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    prompt            TEXT NOT NULL,
    status            job_status NOT NULL DEFAULT 'PENDING_PLAN',
    failure_reason    TEXT,
    failure_detail    JSONB,

    -- durable execution control
    attempt           INT NOT NULL DEFAULT 0,
    max_attempts      INT NOT NULL DEFAULT 3,
    lease_owner       TEXT,
    lease_expires_at  TIMESTAMPTZ,
    run_after         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- budget
    token_budget      BIGINT NOT NULL DEFAULT 150000,
    tokens_used       BIGINT NOT NULL DEFAULT 0,
    cost_usd_micros   BIGINT NOT NULL DEFAULT 0,

    -- linkage, populated by later weeks
    sandbox_id        TEXT,
    repo_url          TEXT,
    pr_url            TEXT,
    preview_url       TEXT,
    artifact_key      TEXT,

    trace_id          TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at        TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The dispatcher's hot path (queue.Claim). Partial index keeps it tiny.
CREATE INDEX idx_jobs_claimable ON jobs (run_after, created_at)
    WHERE status IN ('PENDING_PLAN','QUEUED');

-- The lease-reclaim sweep (queue.sweep).
CREATE INDEX idx_jobs_expired_lease ON jobs (lease_expires_at)
    WHERE status IN ('PLANNING','RUNNING','DEPLOYING');

CREATE INDEX idx_jobs_user_created ON jobs (user_id, created_at DESC);
