-- Week 8: PRD §11.3's options.create_repo, gating PolicyEngine rule 5
-- (§16.3) for the PRIVILEGED git_push/github_open_pr tools. Was
-- already read by the policy engine as a placeholder config value
-- before any PRIVILEGED tool existed to make it reachable — this is
-- what makes it a real per-job value.
ALTER TABLE jobs ADD COLUMN create_repo BOOLEAN NOT NULL DEFAULT false;
