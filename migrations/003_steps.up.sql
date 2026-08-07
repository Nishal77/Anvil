CREATE TYPE step_status AS ENUM (
    'PENDING','RUNNING','SUCCEEDED','FAILED','SKIPPED'
);

CREATE TABLE steps (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id        UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    idx           INT NOT NULL,              -- 0-based ordering
    title         TEXT NOT NULL,
    description   TEXT NOT NULL,
    status        step_status NOT NULL DEFAULT 'PENDING',
    repair_count  INT NOT NULL DEFAULT 0,
    turn_count    INT NOT NULL DEFAULT 0,
    error         TEXT,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (job_id, idx)
);
CREATE INDEX ON steps (job_id, idx);
