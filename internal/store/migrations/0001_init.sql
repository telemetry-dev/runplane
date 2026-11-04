-- Times are unix milliseconds. Booleans are 0 or 1.

CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL -- JSON
) STRICT;

CREATE TABLE users (
	id            TEXT PRIMARY KEY,
	email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
	name          TEXT NOT NULL DEFAULT '',
	password_hash TEXT NOT NULL,
	role          TEXT NOT NULL CHECK (role IN ('admin', 'member')),
	created_at    INTEGER NOT NULL,
	last_login_at INTEGER
) STRICT;

CREATE TABLE sessions (
	id_hash      TEXT PRIMARY KEY,
	user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	ip           TEXT NOT NULL DEFAULT '',
	user_agent   TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX sessions_user_id ON sessions (user_id);
CREATE INDEX sessions_expires_at ON sessions (expires_at);

CREATE TABLE invites (
	id          TEXT PRIMARY KEY,
	email       TEXT NOT NULL COLLATE NOCASE,
	role        TEXT NOT NULL CHECK (role IN ('admin', 'member')),
	token_hash  TEXT NOT NULL UNIQUE,
	created_by  TEXT REFERENCES users (id) ON DELETE SET NULL,
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	accepted_at INTEGER
) STRICT;

CREATE TABLE setup_codes (
	code_hash  TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER
) STRICT;

CREATE TABLE github_apps (
	id                 TEXT PRIMARY KEY,
	app_id             INTEGER NOT NULL UNIQUE,
	slug               TEXT NOT NULL,
	name               TEXT NOT NULL,
	client_id          TEXT NOT NULL DEFAULT '',
	owner_login        TEXT NOT NULL DEFAULT '',
	owner_type         TEXT NOT NULL DEFAULT '',
	html_url           TEXT NOT NULL DEFAULT '',
	private_key_enc    BLOB NOT NULL,
	client_secret_enc  BLOB,
	webhook_secret_enc BLOB,
	created_at         INTEGER NOT NULL
) STRICT;

CREATE TABLE manifest_states (
	state_hash   TEXT PRIMARY KEY,
	account_type TEXT NOT NULL, -- organization | user
	organization TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE installations (
	id                   INTEGER PRIMARY KEY, -- GitHub installation ID
	app_id               INTEGER NOT NULL REFERENCES github_apps (app_id) ON DELETE CASCADE,
	account_login        TEXT NOT NULL,
	account_id           INTEGER NOT NULL,
	account_type         TEXT NOT NULL, -- Organization | User
	avatar_url           TEXT NOT NULL DEFAULT '',
	repository_selection TEXT NOT NULL DEFAULT 'all', -- all | selected
	suspended_at         INTEGER,
	created_at           INTEGER NOT NULL,
	updated_at           INTEGER NOT NULL
) STRICT;

CREATE TABLE repositories (
	id              INTEGER PRIMARY KEY, -- GitHub repository ID
	installation_id INTEGER NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
	full_name       TEXT NOT NULL COLLATE NOCASE,
	private         INTEGER NOT NULL,
	archived        INTEGER NOT NULL DEFAULT 0,
	default_branch  TEXT NOT NULL DEFAULT '',
	updated_at      INTEGER NOT NULL
) STRICT;
CREATE INDEX repositories_installation_id ON repositories (installation_id);
CREATE INDEX repositories_full_name ON repositories (full_name);

CREATE TABLE servers (
	id                  TEXT PRIMARY KEY,
	name                TEXT NOT NULL,
	kind                TEXT NOT NULL, -- local | agent
	status              TEXT NOT NULL, -- online | installing | unreachable | draining | disabled
	status_detail       TEXT NOT NULL DEFAULT '',
	address             TEXT NOT NULL DEFAULT '',
	provider            TEXT NOT NULL DEFAULT '',
	os                  TEXT NOT NULL DEFAULT '',
	kernel              TEXT NOT NULL DEFAULT '',
	arch                TEXT NOT NULL DEFAULT '',
	mode                TEXT NOT NULL DEFAULT '',
	vcpu                INTEGER NOT NULL DEFAULT 0,
	memory_mb           INTEGER NOT NULL DEFAULT 0,
	disk_bytes          INTEGER NOT NULL DEFAULT 0,
	disk_kind           TEXT NOT NULL DEFAULT '',
	kvm                 INTEGER NOT NULL DEFAULT 0,
	docker_version      TEXT NOT NULL DEFAULT '',
	sysbox_version      TEXT NOT NULL DEFAULT '',
	firecracker_version TEXT NOT NULL DEFAULT '',
	version             TEXT NOT NULL DEFAULT '',
	secret_hash         TEXT, -- agents only
	last_seen_at        INTEGER,
	created_at          INTEGER NOT NULL,
	updated_at          INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX servers_secret_hash ON servers (secret_hash) WHERE secret_hash IS NOT NULL;
CREATE UNIQUE INDEX servers_local ON servers (kind) WHERE kind = 'local';

CREATE TABLE server_tokens (
	id         TEXT PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	name       TEXT NOT NULL,
	created_by TEXT REFERENCES users (id) ON DELETE SET NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER,
	server_id  TEXT REFERENCES servers (id) ON DELETE SET NULL
) STRICT;

CREATE TABLE images (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	ref        TEXT NOT NULL UNIQUE,
	is_default INTEGER NOT NULL DEFAULT 0,
	source     TEXT NOT NULL DEFAULT 'registry', -- registry | builtin
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX images_default ON images (is_default) WHERE is_default = 1;

CREATE TABLE image_builds (
	image_id   TEXT NOT NULL REFERENCES images (id) ON DELETE CASCADE,
	server_id  TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
	digest     TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL, -- pending | pulling | building | converting | ready | failed
	error      TEXT NOT NULL DEFAULT '',
	size_bytes INTEGER NOT NULL DEFAULT 0,
	path       TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (image_id, server_id)
) STRICT;

CREATE TABLE pools (
	id                 TEXT PRIMARY KEY,
	name               TEXT NOT NULL,
	label              TEXT NOT NULL UNIQUE COLLATE NOCASE,
	arch               TEXT NOT NULL,
	vcpu               INTEGER NOT NULL,
	memory_mb          INTEGER NOT NULL,
	disk_gb            INTEGER NOT NULL,
	image_id           TEXT REFERENCES images (id) ON DELETE SET NULL, -- NULL = the default image
	min_idle           INTEGER NOT NULL DEFAULT 0,
	max_runners        INTEGER NOT NULL,
	time_limit_minutes INTEGER NOT NULL DEFAULT 0, -- 0 = default_time_limit_minutes
	server_ids         TEXT NOT NULL DEFAULT '[]', -- JSON array, empty = all servers
	enabled            INTEGER NOT NULL DEFAULT 1,
	created_at         INTEGER NOT NULL,
	updated_at         INTEGER NOT NULL
) STRICT;

-- No foreign keys: the GitHub code must delete a scale set on GitHub before
-- its row goes, so a cascade must not remove the row first.
CREATE TABLE scale_sets (
	id              TEXT PRIMARY KEY,
	pool_id         TEXT NOT NULL,
	installation_id INTEGER NOT NULL,
	repository_id   INTEGER, -- NULL for an organization
	github_id       INTEGER NOT NULL DEFAULT 0,
	runner_group_id INTEGER NOT NULL DEFAULT 0,
	name            TEXT NOT NULL,
	status          TEXT NOT NULL, -- creating | listening | error | deleting
	error           TEXT NOT NULL DEFAULT '',
	last_poll_at    INTEGER,
	stats_json      TEXT NOT NULL DEFAULT '{}',
	updated_at      INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX scale_sets_target ON scale_sets (pool_id, installation_id, ifnull(repository_id, 0));

CREATE TABLE runner_bundles (
	version     TEXT NOT NULL,
	arch        TEXT NOT NULL,
	sha256      TEXT NOT NULL DEFAULT '',
	patched     INTEGER NOT NULL DEFAULT 0,
	status      TEXT NOT NULL,
	path        TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	end_date    INTEGER,
	released_at INTEGER,
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (version, arch)
) STRICT;

-- Sandboxes and jobs are history: they keep their pool and server IDs after
-- the pool or server goes.
CREATE TABLE sandboxes (
	id             TEXT PRIMARY KEY,
	pool_id        TEXT NOT NULL,
	server_id      TEXT NOT NULL,
	scale_set_id   TEXT NOT NULL DEFAULT '',
	mode           TEXT NOT NULL,
	state          TEXT NOT NULL,
	runner_id      INTEGER NOT NULL DEFAULT 0,
	runner_name    TEXT NOT NULL DEFAULT '',
	ip             TEXT NOT NULL DEFAULT '',
	image_digest   TEXT NOT NULL DEFAULT '',
	runner_version TEXT NOT NULL DEFAULT '',
	job_id         TEXT NOT NULL DEFAULT '',
	debug          INTEGER NOT NULL DEFAULT 0,
	hold_until     INTEGER,
	vcpu           INTEGER NOT NULL,
	memory_mb      INTEGER NOT NULL,
	created_at     INTEGER NOT NULL,
	idle_at        INTEGER,
	busy_at        INTEGER,
	stopped_at     INTEGER,
	exit_reason    TEXT NOT NULL DEFAULT '',
	error          TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX sandboxes_state ON sandboxes (state);
CREATE INDEX sandboxes_runner_name ON sandboxes (runner_name);

CREATE TABLE jobs (
	id                TEXT PRIMARY KEY,
	runner_request_id INTEGER NOT NULL UNIQUE,
	github_job_id     INTEGER NOT NULL DEFAULT 0,
	scale_set_id      TEXT NOT NULL DEFAULT '',
	pool_id           TEXT NOT NULL DEFAULT '',
	sandbox_id        TEXT NOT NULL DEFAULT '',
	server_id         TEXT NOT NULL DEFAULT '',
	repository        TEXT NOT NULL DEFAULT '',
	workflow_ref      TEXT NOT NULL DEFAULT '',
	workflow_file     TEXT NOT NULL DEFAULT '',
	workflow_name     TEXT NOT NULL DEFAULT '',
	job_name          TEXT NOT NULL DEFAULT '',
	run_id            INTEGER NOT NULL DEFAULT 0,
	run_number        INTEGER NOT NULL DEFAULT 0,
	run_attempt       INTEGER NOT NULL DEFAULT 0,
	head_branch       TEXT NOT NULL DEFAULT '',
	head_sha          TEXT NOT NULL DEFAULT '',
	commit_message    TEXT NOT NULL DEFAULT '',
	event             TEXT NOT NULL DEFAULT '',
	actor             TEXT NOT NULL DEFAULT '',
	pr_number         INTEGER NOT NULL DEFAULT 0,
	labels_json       TEXT NOT NULL DEFAULT '[]',
	state             TEXT NOT NULL, -- queued | assigned | running | completed
	result            TEXT, -- succeeded | failed | cancelled | skipped | NULL
	reject_reason     TEXT NOT NULL DEFAULT '',
	queued_at         INTEGER NOT NULL,
	assigned_at       INTEGER,
	started_at        INTEGER,
	finished_at       INTEGER,
	html_url          TEXT NOT NULL DEFAULT '',
	steps_json        TEXT NOT NULL DEFAULT '[]',
	log_state         TEXT NOT NULL DEFAULT 'none', -- none | live | stored | expired
	cpu_peak          REAL,
	cpu_avg           REAL,
	mem_peak_mb       INTEGER,
	debug             INTEGER NOT NULL DEFAULT 0,
	enriched_at       INTEGER
) STRICT;
CREATE INDEX jobs_queued_at ON jobs (queued_at, id);
CREATE INDEX jobs_state ON jobs (state);
CREATE INDEX jobs_repository ON jobs (repository);
CREATE INDEX jobs_pool_id ON jobs (pool_id);
CREATE INDEX jobs_server_id ON jobs (server_id);

CREATE TABLE job_metrics (
	job_id      TEXT NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
	t_ms        INTEGER NOT NULL,
	cpu_percent REAL NOT NULL,
	memory_mb   INTEGER NOT NULL,
	PRIMARY KEY (job_id, t_ms)
) STRICT, WITHOUT ROWID;

CREATE TABLE cache_entries (
	id           TEXT PRIMARY KEY,
	repository   TEXT NOT NULL,
	ref          TEXT NOT NULL,
	key          TEXT NOT NULL,
	version      TEXT NOT NULL,
	size_bytes   INTEGER NOT NULL DEFAULT 0,
	path         TEXT NOT NULL DEFAULT '',
	finalized    INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX cache_entries_key ON cache_entries (repository, ref, key, version);
CREATE INDEX cache_entries_last_used_at ON cache_entries (last_used_at);

CREATE TABLE cache_stats (
	hour          INTEGER NOT NULL, -- unix hour
	repository    TEXT NOT NULL,
	hits          INTEGER NOT NULL DEFAULT 0,
	misses        INTEGER NOT NULL DEFAULT 0,
	bytes_read    INTEGER NOT NULL DEFAULT 0,
	bytes_written INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (hour, repository)
) STRICT, WITHOUT ROWID;

CREATE TABLE server_stats (
	server_id      TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
	t_ms           INTEGER NOT NULL,
	cpu_percent    REAL NOT NULL,
	memory_percent REAL NOT NULL,
	disk_percent   REAL NOT NULL,
	PRIMARY KEY (server_id, t_ms)
) STRICT, WITHOUT ROWID;
CREATE INDEX server_stats_t_ms ON server_stats (t_ms);

CREATE TABLE notification_channels (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	kind        TEXT NOT NULL, -- slack | discord | webhook
	url_enc     BLOB NOT NULL,
	events_json TEXT NOT NULL DEFAULT '[]',
	enabled     INTEGER NOT NULL DEFAULT 1,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
) STRICT;

CREATE TABLE backups (
	id         TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	path       TEXT NOT NULL DEFAULT '',
	size_bytes INTEGER NOT NULL DEFAULT 0,
	s3_key     TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	error      TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE audit_log (
	id          TEXT PRIMARY KEY,
	at          INTEGER NOT NULL,
	user_id     TEXT NOT NULL DEFAULT '',
	action      TEXT NOT NULL,
	target      TEXT NOT NULL DEFAULT '',
	detail_json TEXT NOT NULL DEFAULT '{}',
	ip          TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX audit_log_at ON audit_log (at, id);
