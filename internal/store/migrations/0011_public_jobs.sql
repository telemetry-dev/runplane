-- The scheduler records if the repository of a job is public. After an admin
-- turns allow_public_repos off, a public job that a scale set acquired before
-- can still wait for a runner, and only a server in VM mode can give it one.
-- The rows from before this column count as private.
ALTER TABLE jobs ADD COLUMN public INTEGER NOT NULL DEFAULT 0;
