-- API changes: the server region, the last doctor result of each server, and
-- the last delivery of each notification channel.

ALTER TABLE servers ADD COLUMN region TEXT NOT NULL DEFAULT '';

CREATE TABLE server_doctor (
	server_id   TEXT PRIMARY KEY REFERENCES servers (id) ON DELETE CASCADE,
	checked_at  INTEGER NOT NULL,
	checks_json TEXT NOT NULL DEFAULT '[]'
) STRICT;

-- last_error is empty when the last delivery worked.
ALTER TABLE notification_channels ADD COLUMN last_sent_at INTEGER;
ALTER TABLE notification_channels ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
