
ALTER TABLE gitlab_connection
    DROP COLUMN webhook_secret_encrypted,
    DROP COLUMN access_token_encrypted;
