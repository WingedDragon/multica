ALTER TABLE gitlab_connection
    ADD COLUMN access_token_encrypted TEXT NOT NULL DEFAULT '',
    ADD COLUMN webhook_secret_encrypted TEXT NOT NULL DEFAULT '';
