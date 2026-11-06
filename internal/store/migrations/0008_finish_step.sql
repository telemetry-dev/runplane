-- A completed job waits for its finish step, which stores the GitHub log.
-- Before this column, log_state live was also the mark of that wait. A job
-- that ran without that log state got no new finish step after a restart,
-- and a job could keep live after its sandbox stopped.
ALTER TABLE jobs ADD COLUMN finish_pending INTEGER NOT NULL DEFAULT 0;
UPDATE jobs SET finish_pending = 1, log_state = 'none' WHERE state = 'completed' AND log_state = 'live';
CREATE INDEX jobs_finish_pending ON jobs (finished_at) WHERE finish_pending;

-- A manifest state keeps the GitHub account type of the new app owner.
UPDATE manifest_states SET account_type = 'Organization' WHERE account_type = 'organization';
UPDATE manifest_states SET account_type = 'User' WHERE account_type = 'user';
