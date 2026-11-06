-- No code reads the address of a sandbox from its row. The cache server and
-- the registry mirror find a sandbox in the list of the driver.
ALTER TABLE sandboxes DROP COLUMN ip;
