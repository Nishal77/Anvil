CREATE TABLE dead_letter_jobs (
    job_id      UUID PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    attempts    INT NOT NULL,
    last_error  TEXT NOT NULL,
    snapshot    JSONB NOT NULL,            -- full job+steps at time of death
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
