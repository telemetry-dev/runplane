-- The error of the last failed agent update has its own column. Before, it
-- was in status_detail, and the other writers of status_detail replaced it.
ALTER TABLE servers ADD COLUMN update_error TEXT NOT NULL DEFAULT '';
