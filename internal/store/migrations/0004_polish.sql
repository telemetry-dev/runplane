-- The base branch of a pull request run. The cache server takes the refs
-- of a job from event, head_branch, pr_number, and base_ref.
ALTER TABLE jobs ADD COLUMN base_ref TEXT NOT NULL DEFAULT '';

-- The disk size of a sandbox: the scheduler replaces a warm sandbox that no
-- longer has the size of its pool.
ALTER TABLE sandboxes ADD COLUMN disk_gb INTEGER NOT NULL DEFAULT 0;
UPDATE sandboxes SET disk_gb = coalesce((SELECT p.disk_gb FROM pools p WHERE p.id = sandboxes.pool_id), 0);
