CREATE TYPE event_type AS ENUM (
    'job_created','plan_ready','plan_approved',
    'step_started','step_finished',
    'tool_call','log_line','stream_gap',
    'deploy_started','deploy_ready',
    'job_finished','error'
);

CREATE TABLE job_events (
    job_id     UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    seq        BIGINT NOT NULL,
    type       event_type NOT NULL,
    payload    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, seq)
);
-- Range scans on reconnect replay are the only read pattern.
CREATE INDEX ON job_events (created_at);   -- for the archival sweep

-- Per-job monotonic sequence, allocated inside the same tx as the insert.
CREATE TABLE job_event_seq (
    job_id   UUID PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    last_seq BIGINT NOT NULL DEFAULT 0
);
