-- agent_turns is the full audit trail of model interaction (PRD §10,
-- FR-025). policy_decision and policy_reason are additive to the
-- PRD's literal CREATE TABLE: §16.3 requires every policy decision,
-- including Allow, to be persisted, and the base schema had no column
-- to hold it — flagged and added rather than worked around.
CREATE TABLE agent_turns (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id            UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    step_id           UUID REFERENCES steps(id) ON DELETE CASCADE,
    turn_idx          INT NOT NULL,
    role              TEXT NOT NULL,            -- 'planner' | 'executor'
    model             TEXT NOT NULL,
    provider          TEXT NOT NULL,
    prompt_sha256     BYTEA NOT NULL,           -- hash, not the prompt (I-3)
    prompt_ref        TEXT,                     -- object storage key if retained
    tool_name         TEXT,
    tool_args         JSONB,
    policy_decision   TEXT NOT NULL,            -- 'ALLOW' | 'DENY' | 'REQUIRE_APPROVAL'
    policy_reason     TEXT,
    observation       TEXT,
    tokens_in         INT NOT NULL DEFAULT 0,
    tokens_out        INT NOT NULL DEFAULT 0,
    cost_usd_micros   BIGINT NOT NULL DEFAULT 0,
    latency_ms        INT,
    error             TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Serves the executor's own reload-on-reclaim query (ordered turn
-- history for one job) and the audit query behind Week 6's acceptance
-- criterion "every tool call appears in agent_turns with its policy
-- decision".
CREATE INDEX ON agent_turns (job_id, turn_idx);
