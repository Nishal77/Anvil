CREATE TABLE idempotency_keys (
    key         TEXT PRIMARY KEY,          -- sha256(job_id|step_id|tool|args)
    job_id      UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    result      JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON idempotency_keys (created_at);
