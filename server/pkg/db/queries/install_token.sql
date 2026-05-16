-- name: CreateInstallToken :one
-- §6.4 / D5: mint a short-lived single-use install token (`mit_` prefix).
-- Caller-supplied expires_at lets the handler enforce the 15-minute window.
INSERT INTO install_token (token_hash, workspace_id, created_by_user_id, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ConsumeInstallToken :one
-- Exchange path: atomically flip used_at on a token that is still live
-- (unexpired and never used). A second call against the same hash matches zero
-- rows, which the handler turns into 401 install_token_already_used.
UPDATE install_token
SET used_at = now()
WHERE token_hash = $1
  AND expires_at > now()
  AND used_at IS NULL
RETURNING *;

-- name: GetInstallToken :one
SELECT * FROM install_token
WHERE token_hash = $1;

-- name: DeleteExpiredInstallTokens :exec
DELETE FROM install_token
WHERE expires_at <= now() - interval '1 day';
