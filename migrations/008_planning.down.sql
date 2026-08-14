ALTER TABLE steps DROP COLUMN optional;
ALTER TABLE steps DROP COLUMN acceptance;
DROP INDEX IF EXISTS idx_jobs_cancel_requested;
ALTER TABLE jobs DROP COLUMN cancel_requested_at;
ALTER TABLE jobs DROP COLUMN auto_approve;
ALTER TABLE jobs DROP COLUMN plan_risks;
ALTER TABLE jobs DROP COLUMN plan_summary;
