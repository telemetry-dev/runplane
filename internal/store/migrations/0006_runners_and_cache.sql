-- The end date warning of a runner version goes out one time, also after a
-- restart of runplane.
ALTER TABLE runner_bundles ADD COLUMN warned INTEGER NOT NULL DEFAULT 0;

-- No code reads or writes runner_bundles.path: the folder of a runner bundle
-- is a path on its own server. The migrations only add, so the column stays.
