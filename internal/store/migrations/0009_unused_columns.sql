-- No code reads or writes these columns. Each server finds the files of an
-- image, a runner bundle, or a cache entry from its digest, its version, or
-- its ID.
ALTER TABLE image_builds DROP COLUMN path;
ALTER TABLE runner_bundles DROP COLUMN path;
ALTER TABLE cache_entries DROP COLUMN path;

-- The cache server and the registry mirror find a sandbox by its address in
-- the list of the driver, so no query reads the sandboxes by address.
DROP INDEX sandboxes_live_ip;
