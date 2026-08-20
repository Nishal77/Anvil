-- Week 8: envelope-encrypted user secrets (PRD §10, §16.5). Ciphertext
-- and nonce only — the encryption key lives in ANVIL_SECRET_ENCRYPTION_KEY,
-- never in this database, so a Postgres dump alone can never yield a
-- usable secret.
CREATE TABLE user_secrets (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,               -- e.g. 'GITHUB_TOKEN'
    ciphertext    BYTEA NOT NULL,              -- AES-256-GCM
    nonce         BYTEA NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);
