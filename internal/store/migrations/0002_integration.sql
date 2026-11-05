-- The cache server and the registry mirror find the sandbox of each request
-- by its source address. The WHERE clause must stay equal to the one in
-- sandboxJobQuery in sandboxes.go, so that SQLite uses this index.
CREATE INDEX sandboxes_live_ip ON sandboxes (ip) WHERE state NOT IN ('gone', 'failed');
