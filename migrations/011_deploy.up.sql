-- Week 9: PRD §11.3's options.deploy. preview_url already exists on
-- jobs (migrations/002_jobs.up.sql, "populated by later weeks") — this
-- is the later week; only the missing deploy flag needs adding.
ALTER TABLE jobs ADD COLUMN deploy BOOLEAN NOT NULL DEFAULT false;
