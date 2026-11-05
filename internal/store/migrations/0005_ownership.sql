-- runplane guesses the result of a job whose sandbox went away and of a job
-- that it did not take. A result from GitHub replaces a guessed result.
-- Before this column, the store replaced each cancelled result.
ALTER TABLE jobs ADD COLUMN result_guessed INTEGER NOT NULL DEFAULT 0;
UPDATE jobs SET result_guessed = 1 WHERE result = 'cancelled';

-- No code reads the client secret of the GitHub App, and the app has no
-- webhook.
ALTER TABLE github_apps DROP COLUMN client_secret_enc;
ALTER TABLE github_apps DROP COLUMN webhook_secret_enc;
