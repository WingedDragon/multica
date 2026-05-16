DROP TABLE IF EXISTS install_token;

ALTER TABLE daemon_token
    DROP COLUMN IF EXISTS revoked_at;
